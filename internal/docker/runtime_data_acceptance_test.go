package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/nativefiles"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/mbianchidev/porto/internal/vm"
)

type runtimeDataVMRunner struct {
	runtimes.ExecRunner
	runAs string
}

func (runner runtimeDataVMRunner) command(command runtimes.Command) runtimes.Command {
	if len(command.Args) > 0 && command.Args[0] == "create" {
		command.Args = append([]string{"create", "--mount-none", "--containerd=none"}, command.Args[1:]...)
		if runtime.GOARCH == "arm64" && os.Getenv("PORTO_DOCKER_CLIENT_SOFTWARE_EMULATION") == "1" {
			command.Args = append(command.Args, `--set=.vmOpts.qemu.cpuType.aarch64 = "neoverse-n1"`)
		}
		for index, argument := range command.Args {
			if argument == "template:ubuntu-24.04" {
				command.Args[index] = "template:ubuntu-26.04"
			}
		}
	}
	if runner.runAs != "" {
		arguments := []string{"-u", runner.runAs, "--", "env", "PATH=" + os.Getenv("PATH"), "LIMA_HOME=" + os.Getenv("LIMA_HOME"), command.Name}
		command.Name = "sudo"
		command.Args = append(arguments, command.Args...)
	}
	return command
}

func (runner runtimeDataVMRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	output, err := runner.ExecRunner.Run(ctx, runner.command(command))
	return runtimeDataVMDiagnostics(command, output, err)
}

func (runner runtimeDataVMRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	output, err := runner.ExecRunner.RunStreaming(ctx, runner.command(command), emit)
	return runtimeDataVMDiagnostics(command, output, err)
}

func runtimeDataVMDiagnostics(command runtimes.Command, output []byte, err error) ([]byte, error) {
	if err == nil || len(command.Args) < 2 || command.Args[0] != "start" {
		return output, err
	}
	name := command.Args[len(command.Args)-1]
	home := os.Getenv("LIMA_HOME")
	if home == "" || filepath.Base(name) != name || !strings.HasPrefix(name, "porto-data-") {
		return output, err
	}
	diagnostics, diagnosticErr := os.ReadFile(filepath.Join(home, name, "ha.stderr.log"))
	if diagnosticErr != nil {
		return append(output, []byte(fmt.Sprintf("\nisolated source VM diagnostics unavailable: %v\n", diagnosticErr))...), err
	}
	output = append(output, []byte("\nisolated source VM diagnostics:\n")...)
	return append(output, diagnostics[max(0, len(diagnostics)-16384):]...), err
}

type runtimeDataFixture struct {
	t                 *testing.T
	ctx               context.Context
	manager           *Manager
	store             *store.Store
	native            *nativefiles.Manager
	vms               *vm.Manager
	instance          vm.Instance
	source            *migrationClient
	root              string
	prefix            string
	helper            string
	destinationHelper string
	vmRunner          runtimeDataVMRunner
	extraSelections   []dataops.Selection
	sourceFault       atomic.Int32
	selection         []dataops.Selection
}

func TestRuntimeDataAcceptance(t *testing.T) {
	if os.Getenv("PORTO_RUNTIME_DATA_ACCEPTANCE") != "1" {
		t.Skip("set PORTO_RUNTIME_DATA_ACCEPTANCE=1 for isolated real Docker migration and OS-native runtime mounts")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	t.Cleanup(cancel)
	manager := New(nil)
	if os.Getenv("PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA") == "1" {
		claimFreshClientLima(t, ctx, manager)
	}
	if os.Getenv("PORTO_DOCKER_CLIENT_BACKEND") == "local" {
		if runtime.GOOS != "linux" {
			t.Fatal("local runtime acceptance requires Linux")
		}
		manager = NewWithStateDir(nil, t.TempDir())
		if err := manager.writeEngineState(engineState{Mode: "direct", CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	} else {
		ownership := manager.EngineOwnershipStatus(ctx)
		if !ownership.Owned || !ownership.Verified {
			t.Fatalf("an already-running, verified Porto destination is required: %s", ownership.Message)
		}
	}
	if override := os.Getenv("PORTO_STORAGE_TEST_HELPER"); override != "" {
		manager.runner = storageIntegrationRunner{helper: override}
	}
	root, err := os.MkdirTemp(runtimeDataTemporaryRoot(), "porto-data-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove exact runtime acceptance directory: %v", err)
		}
	})
	t.Setenv("PORTO_HOME", filepath.Join(root, "state"))
	ledger, err := store.Open(filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Error(err)
		}
	})
	manager.SetMigrationVolumeLedger(ledger.RecordMigratedVolume, ledger.KnownMigratedVolume)
	manager.SetMigrationTemporaryLedger(ledger.ReserveMigrationTemporary, ledger.ClearMigrationTemporary, ledger.MigrationTemporaries)
	manager.SetVolumeTransferLedger(ledger.ReserveVolumeTemporary, ledger.ClearVolumeTemporary, ledger.VolumeTemporaries)
	native := nativefiles.New(filepath.Join(root, "native paths-\u03b4"), nil)
	manager.SetNativeFilesGuard(native.Guard)
	manager.SetNativeFilesClose(native.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := native.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper, err := runtimes.BundledRuntimeHelper(executable, runtimes.LookPath)
	if err != nil || helper == "" {
		t.Fatalf("build the current Linux helper beside the acceptance test in runtime/bin: %v", err)
	}
	fixture := &runtimeDataFixture{
		t: t, ctx: ctx, manager: manager, store: ledger, native: native, root: root,
		prefix: "porto-data-" + newInventoryInstanceID()[:12], helper: helper,
	}
	t.Logf("isolated runtime acceptance fixture: %s", fixture.prefix)
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		account, err := user.Lookup(os.Getenv("SUDO_USER"))
		if err != nil {
			t.Fatalf("run rootful acceptance through sudo from a real user so Lima never runs as root: %v", err)
		}
		uid, err := strconv.Atoi(account.Uid)
		if err != nil {
			t.Fatal(err)
		}
		gid, err := strconv.Atoi(account.Gid)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(root, uid, gid); err != nil {
			t.Fatal(err)
		}
		t.Setenv("LIMA_HOME", filepath.Join(root, "lima"))
		fixture.vmRunner.runAs = account.Username
	}
	fixture.useCurrentDestinationHelper()
	fixture.prepareSource()
	fixture.prepareObjects()
	t.Run("selected migration and source preservation", fixture.migrateSelected)
	if t.Failed() {
		return
	}
	t.Run("transfer failure and interruption recovery", fixture.migrationFailures)
	if t.Failed() {
		return
	}
	t.Run("real native resource mounts", fixture.nativeResources)
	if t.Failed() {
		return
	}
	t.Run("native lifecycle boundaries", fixture.nativeLifecycles)
	if t.Failed() {
		return
	}
	t.Run("owned engine update and shutdown", fixture.engineLifecycle)
}

func (fixture *runtimeDataFixture) useCurrentDestinationHelper() {
	if os.Getenv("PORTO_RUNTIME_DATA_USE_CURRENT_HELPER") != "1" {
		return
	}
	state, err := fixture.manager.readEngineState()
	if err != nil || state.Mode != "lima" {
		fixture.t.Fatalf("the non-replacing helper override requires an owned Lima destination: %v", err)
	}
	path := "/tmp/" + fixture.prefix + "-helper"
	runner := runtimes.ExecRunner{}
	for _, arguments := range [][]string{
		{"copy", fixture.helper, state.Instance + ":" + path},
		{"shell", "--workdir=/", state.Instance, "--", "chmod", "700", path},
	} {
		if output, err := runner.Run(fixture.ctx, runtimes.Command{Name: "limactl", Args: arguments}); err != nil {
			fixture.t.Fatalf("copy current test helper without replacing the installed helper: %v: %s", err, output)
		}
	}
	fixture.destinationHelper = path
	fixture.manager.runner = storageIntegrationRunner{helper: path}
	fixture.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if output, err := runner.Run(ctx, runtimes.Command{Name: "limactl", Args: []string{"shell", "--workdir=/", state.Instance, "--", "rm", "--", path}}); err != nil {
			fixture.t.Errorf("remove exact test helper: %v: %s", err, output)
		}
	})
}

func runtimeDataTemporaryRoot() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return "/tmp"
}

func (fixture *runtimeDataFixture) prepareSource() {
	t := fixture.t
	runner := fixture.vmRunner
	fixture.vms = vm.NewWithStateDir(runner, filepath.Join(fixture.root, "vms"))
	vmType := os.Getenv("PORTO_RUNTIME_DATA_VM_TYPE")
	if vmType == "" && runtime.GOOS != "darwin" {
		vmType = "qemu"
	}
	instance, err := fixture.vms.Create(fixture.ctx, vm.CreateRequest{
		Name: fixture.prefix, Owner: fixture.prefix, Image: "ubuntu-24.04", VMType: vmType,
		CPUs: 2, MemoryMiB: 1536, DiskGiB: 12, Start: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.instance = instance
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := fixture.vms.EnsureStandalone(instance.Name); err != nil {
			t.Error(err)
			return
		}
		if err := fixture.vms.Delete(cleanup, instance.Name, true); err != nil {
			t.Error(err)
		}
	})
	const provision = `set -eu
sudo -n apt-get update -qq
sudo -n env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io
sudo -n systemctl start docker
sudo -n docker version --format '{{.Server.Version}}'
`
	if output, err := fixture.vms.Exec(fixture.ctx, instance.Name, []string{"sh", "-c", provision}, nil); err != nil {
		t.Fatalf("prepare isolated Docker source: %v: %.8192s", err, output)
	}
	guestHelper := "/tmp/" + fixture.prefix + "-helper"
	for _, command := range []runtimes.Command{
		{Name: "limactl", Args: []string{"copy", fixture.helper, instance.Name + ":" + guestHelper}},
		{Name: "limactl", Args: []string{"shell", "--workdir=/", instance.Name, "--", "chmod", "700", guestHelper}},
	} {
		if output, err := runner.Run(fixture.ctx, command); err != nil {
			t.Fatalf("install isolated source transport: %v: %s", err, output)
		}
	}
	endpoint := filepath.Join(fixture.root, "source.sock")
	sourceURL := "unix://" + endpoint
	if runtime.GOOS == "windows" {
		endpoint = `\\.\pipe\` + fixture.prefix
		sourceURL = "npipe:////./pipe/" + fixture.prefix
	}
	listener, lease, err := listenDockerEndpoint(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("http://docker")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = fixture.sourceResponse
	proxy.Transport = &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		spec := runner.command(runtimes.Command{Name: "limactl", Args: []string{"shell", "--workdir=/", instance.Name, "--", "sudo", "-n", guestHelper, "dial-stdio", "/var/run/docker.sock"}})
		command := newTunnelCommand(ctx, spec.Name, spec.Args...)
		command.Env = process.WithEnvironment(os.Environ(), spec.Env...)
		return dialCommandConn(ctx, "isolated Docker source", command, buildKitAddr("source-fixture"), buildKitAddr("docker"))
	}}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
		if err := lease.Release(endpoint); err != nil {
			t.Error(err)
		}
	})
	fixture.source = &migrationClient{
		context: MigrationContext{Name: "desktop-" + fixture.prefix, Endpoint: sourceURL, Supported: true, Desktop: true},
		client: &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialMigrationSource(ctx, sourceURL)
		}}},
	}
	config := filepath.Join(fixture.root, "docker-config")
	if err := os.Mkdir(config, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", config)
	data := []byte(`{"auths":{"synthetic.invalid":{"auth":"c3ludGhldGljOnN5bnRoZXRpYw=="}}}`)
	if err := os.WriteFile(filepath.Join(config, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := (runtimes.ExecRunner{}).Run(fixture.ctx, runtimes.Command{Name: "docker", Args: []string{
		"context", "create", fixture.source.context.Name, "--docker", "host=" + sourceURL,
	}}); err != nil {
		t.Fatalf("create isolated source context: %v: %s", err, output)
	}
	contexts, err := fixture.manager.MigrationContexts(fixture.ctx)
	if err != nil || !slices.ContainsFunc(contexts, func(item MigrationContext) bool {
		return item.Name == fixture.source.context.Name && item.Desktop && item.Supported
	}) {
		t.Fatalf("real local source context was not discovered: %+v, %v", contexts, err)
	}
}

func (fixture *runtimeDataFixture) sourceJSON(method, resource string, query url.Values, input any, result any) {
	fixture.t.Helper()
	var body io.Reader
	if input != nil {
		document, err := json.Marshal(input)
		if err != nil {
			fixture.t.Fatal(err)
		}
		body = bytes.NewReader(document)
	}
	response, err := fixture.source.request(fixture.ctx, method, resource, query, body)
	if err != nil {
		fixture.t.Fatal(err)
	}
	var decodeErr error
	if result != nil {
		decodeErr = json.NewDecoder(response.Body).Decode(result)
	} else {
		_, decodeErr = io.Copy(io.Discard, response.Body)
	}
	if err := errors.Join(decodeErr, response.Body.Close()); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *runtimeDataFixture) prepareObjects() {
	t := fixture.t
	response, err := fixture.source.request(fixture.ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {"busybox"}, "tag": {"1.37.0"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	readClientProgress(t, response.Body)
	seed := struct {
		ID string `json:"Id"`
	}{}
	fixture.sourceJSON(http.MethodPost, "/containers/create", nil, map[string]any{
		"Image": "busybox:1.37.0", "Cmd": []string{"sh", "-c", "sleep 600"},
		"Labels":     map[string]string{"porto.test.suite": fixture.prefix},
		"HostConfig": map[string]any{"NetworkMode": "none"},
	}, &seed)
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("synthetic runtime file\n")
	if err := writer.WriteHeader(&tar.Header{Name: "synthetic.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ name, body string }{
		{"Case.txt", "upper case fixture"}, {"case.txt", "lower case fixture"},
	} {
		if err := writer.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, entry.body); err != nil {
			t.Fatal(err)
		}
	}
	for _, header := range []*tar.Header{
		{Name: "inside-link", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "synthetic.txt"},
		{Name: "escape-link", Typeflag: tar.TypeSymlink, Mode: 0o777, Linkname: "/tmp/" + fixture.prefix + "-outside.txt"},
		{Name: "hardlink.txt", Typeflag: tar.TypeLink, Mode: 0o640, Linkname: "synthetic.txt"},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, err = fixture.source.request(fixture.ctx, http.MethodPut, "/containers/"+seed.ID+"/archive", url.Values{"path": {"/"}}, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	var committed struct {
		ID string `json:"Id"`
	}
	fixture.sourceJSON(http.MethodPost, "/commit", url.Values{"container": {seed.ID}, "repo": {fixture.prefix}, "tag": {"fixture"}}, nil, &committed)
	fixture.sourceJSON(http.MethodPost, "/images/"+url.PathEscape(committed.ID)+"/tag", url.Values{"repo": {fixture.prefix}, "tag": {"alias"}}, nil, nil)
	fixture.sourceJSON(http.MethodDelete, "/containers/"+seed.ID, nil, nil, nil)
	fixture.sourceJSON(http.MethodDelete, "/images/busybox:1.37.0", nil, nil, nil)
	volume := fixture.prefix + "-source"
	fixture.sourceJSON(http.MethodPost, "/volumes/create", nil, map[string]any{"Name": volume, "Labels": map[string]string{
		"porto.test.suite": fixture.prefix, "com.docker.compose.project": fixture.prefix,
	}}, nil)
	network := fixture.prefix + "-network"
	var createdNetwork struct {
		ID string `json:"Id"`
	}
	fixture.sourceJSON(http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": network, "Driver": "bridge", "Internal": false, "Labels": map[string]string{"porto.test.suite": fixture.prefix},
	}, &createdNetwork)
	var container struct {
		ID string `json:"Id"`
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hostPort := strconv.Itoa(reservation.Addr().(*net.TCPAddr).Port)
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.sourceJSON(http.MethodPost, "/containers/create", url.Values{"name": {fixture.prefix + "-app"}}, map[string]any{
		"Image": fixture.prefix + ":fixture", "Cmd": []string{"sh", "-c", "printf 'porto-runtime-native-fixture\\n' > /tmp/index.html; exec httpd -f -p 8080 -h /tmp"},
		"Env": []string{"SYNTHETIC_MODE=acceptance"}, "WorkingDir": "/tmp", "Hostname": "synthetic-host",
		"Labels":       map[string]string{"porto.test.suite": fixture.prefix, "com.docker.compose.project": fixture.prefix, "com.docker.compose.service": "app"},
		"Healthcheck":  map[string]any{"Test": []string{"CMD-SHELL", "test -f /synthetic.txt"}, "Interval": int64(30 * time.Second), "Timeout": int64(3 * time.Second), "Retries": 2},
		"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
		"HostConfig": map[string]any{
			"Mounts": []map[string]any{
				{"Type": "volume", "Source": volume, "Target": "/data", "VolumeOptions": map[string]bool{"NoCopy": true}},
				{"Type": "volume", "Source": volume, "Target": "/readonly", "ReadOnly": true, "VolumeOptions": map[string]bool{"NoCopy": true}},
			},
			"NetworkMode": network, "RestartPolicy": map[string]any{"Name": "on-failure", "MaximumRetryCount": 2},
			"Memory": int64(128 * 1024 * 1024), "MemorySwap": int64(256 * 1024 * 1024), "NanoCpus": int64(500_000_000),
			"PortBindings": map[string]any{"8080/tcp": []map[string]string{{"HostIP": "127.0.0.1", "HostPort": hostPort}}},
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{network: map[string]any{"Aliases": []string{"synthetic-host"}}}},
	}, &container)
	var volumePayload bytes.Buffer
	volumeWriter := tar.NewWriter(&volumePayload)
	reader := tar.NewReader(bytes.NewReader(archive.Bytes()))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "escape-link" {
			continue
		}
		if err := volumeWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(volumeWriter, reader); err != nil {
			t.Fatal(err)
		}
	}
	if err := volumeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	response, err = fixture.source.request(fixture.ctx, http.MethodPut, "/containers/"+container.ID+"/archive", url.Values{"path": {"/data"}}, bytes.NewReader(volumePayload.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	inventory, err := fixture.manager.MigrationInventory(fixture.ctx, fixture.source.context.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range inventory.Objects {
		if !strings.HasPrefix(object.Name, fixture.prefix) {
			continue
		}
		destination := object.Name
		if object.Kind != "image" {
			destination = fixture.prefix + "-destination-" + object.Kind
		}
		fixture.selection = append(fixture.selection, dataops.Selection{Kind: object.Kind, Name: object.Name, ID: object.ID, Destination: destination})
	}
	if len(fixture.selection) != 4 {
		t.Fatalf("isolated source inventory=%+v", inventory.Objects)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := fixture.native.Close(ctx); err != nil {
			t.Errorf("detach real host folders before deleting fixture resources: %v", err)
			return
		}
		for _, kind := range []string{"container", "volume", "network", "image"} {
			for _, selection := range append(slices.Clone(fixture.selection), fixture.extraSelections...) {
				if selection.Kind != kind {
					continue
				}
				err := fixture.removeDestination(ctx, selection)
				if err != nil && !errorsIsNotFound(err) && !missingDockerObject(err, kind) {
					t.Errorf("remove exact acceptance destination %s: %v", selection.Destination, err)
				}
			}
			for _, tag := range []string{fixture.prefix + ":fixture", fixture.prefix + ":alias"} {
				if err := fixture.removeDestination(ctx, dataops.Selection{Kind: "image", Destination: tag}); err != nil && !missingDockerObject(err, "image") {
					t.Error(err)
				}
			}
		}
	})
}

func (fixture *runtimeDataFixture) migrateSelected(t *testing.T) {
	request := dataops.Request{Action: "migration", Context: fixture.source.context.Name, Selections: fixture.selection}
	before := make(map[string]json.RawMessage)
	for _, selection := range fixture.selection {
		document, err := fixture.source.inspect(fixture.ctx, selection)
		if err != nil {
			t.Fatal(err)
		}
		before[selection.Kind] = document
	}
	var imageIdentity struct {
		ID               string `json:"Id"`
		Descriptor       struct{ Digest, MediaType string }
		Architecture, Os string
	}
	if err := json.Unmarshal(before["image"], &imageIdentity); err != nil {
		t.Fatal(err)
	}
	t.Logf("source image identity: %+v; destination CLI-only=%v", imageIdentity, fixture.manager.directCLI)
	configPath := filepath.Join(os.Getenv("DOCKER_CONFIG"), "config.json")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
	if err != nil || len(preview.Conflicts) != 0 {
		t.Fatalf("real migration dry run: %+v, %v", preview.Conflicts, err)
	}
	preview.Request.Confirm = true
	transfers := filepath.Join(os.Getenv("PORTO_HOME"), "transfers")
	if err := os.MkdirAll(transfers, 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.BeginDataOperation(fixture.ctx, preview.Request, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.manager.Migrate(fixture.ctx, preview.Request, transfers, func(string, int64) error { return nil }, func(result dataops.Result) error {
		return fixture.store.CheckpointDataOperation(fixture.ctx, run.ID, result)
	})
	if err != nil {
		_ = fixture.store.FinishDataOperation(fixture.ctx, run.ID, "failed", result, err.Error(), time.Now())
		for _, selection := range fixture.selection {
			if selection.Kind == "image" {
				current, inspectErr := fixture.manager.InspectImage(fixture.ctx, selection.Destination, "")
				t.Logf("synthetic source image metadata: %s", before["image"])
				t.Logf("synthetic destination image metadata: %s; inspect error: %v", current, inspectErr)
			}
		}
		if errors.Is(err, datafiles.ErrConflict) && len(result.Steps) == 0 {
			for _, selection := range fixture.selection {
				current, inspectErr := fixture.source.inspect(fixture.ctx, selection)
				if inspectErr != nil {
					t.Log(inspectErr)
					continue
				}
				var original, now map[string]json.RawMessage
				_ = json.Unmarshal(before[selection.Kind], &original)
				_ = json.Unmarshal(current, &now)
				for key, value := range original {
					if !bytes.Equal(value, now[key]) {
						t.Logf("synthetic changed preview metadata: %s.%s", selection.Kind, key)
					}
				}
			}
		}
		t.Fatalf("selected real-object migration: %+v, %v", result, err)
	}
	if err := fixture.store.FinishDataOperation(fixture.ctx, run.ID, "succeeded", result, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) != 4 || slices.ContainsFunc(result.Steps, func(step dataops.Step) bool { return step.Status != "succeeded" }) {
		t.Fatalf("migration lost selected outcomes: %+v", result)
	}
	for _, selection := range fixture.selection {
		after, err := fixture.source.inspect(fixture.ctx, selection)
		originalDigest, originalErr := migrationPlanDigest(before[selection.Kind])
		currentDigest, currentErr := migrationPlanDigest(after)
		if err != nil || originalErr != nil || currentErr != nil || originalDigest != currentDigest {
			var previous, current map[string]json.RawMessage
			_ = json.Unmarshal(before[selection.Kind], &previous)
			_ = json.Unmarshal(after, &current)
			for key, value := range previous {
				if !bytes.Equal(value, current[key]) {
					t.Logf("synthetic changed source metadata: %s.%s", selection.Kind, key)
				}
			}
			t.Fatalf("source %s configuration changed: %v", selection.Kind, err)
		}
		if selection.Kind == "volume" {
			descriptor, err := fixture.manager.FileDescriptor(fixture.ctx, "volume", selection.Destination)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := fixture.manager.FileRequest(fixture.ctx, "volume", selection.Destination, datafiles.Request{Action: "read", Path: "synthetic.txt", Identity: descriptor.Resource.Fingerprint()}, nil, &output); err != nil {
				t.Fatal(err)
			}
			var file datafiles.Content
			if err := json.Unmarshal(output.Bytes(), &file); err != nil || file.Text != "synthetic runtime file\n" {
				t.Fatalf("volume contents did not round trip: %s, %v", output.Bytes(), err)
			}
			document, err := fixture.manager.InspectVolume(fixture.ctx, selection.Destination)
			if err != nil {
				t.Fatal(err)
			}
			var inspected struct{ Labels map[string]string }
			if err := json.Unmarshal(document, &inspected); err != nil || inspected.Labels["porto.test.suite"] != fixture.prefix {
				t.Fatalf("volume labels were not preserved: %s, %v", document, err)
			}
		}
		if selection.Kind == "container" {
			fixture.requireContainerFidelity(t, selection, before["container"])
		}
	}
	currentConfig, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(config, currentConfig) {
		t.Fatalf("source context credentials/config were changed: %v", err)
	}
	retry, err := fixture.manager.PreviewMigration(fixture.ctx, request)
	if err != nil || len(retry.Conflicts) != 0 {
		t.Fatalf("verified repeat was not idempotent: %+v, %v", retry.Conflicts, err)
	}
	image := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "image" })]
	missingAlias := fixture.prefix + ":alias"
	if image.Name == missingAlias {
		missingAlias = fixture.prefix + ":fixture"
	}
	if err := fixture.manager.RemoveImage(fixture.ctx, missingAlias, false); err != nil {
		t.Fatal(err)
	}
	repeated, err := fixture.migrationRun(t, request.Selections, false, nil)
	if err != nil || len(repeated.Steps) != len(request.Selections) || slices.ContainsFunc(repeated.Steps, func(step dataops.Step) bool { return step.Status != "succeeded" }) {
		t.Fatalf("an actual repeated migration did not reuse verified objects: %+v, %v", repeated, err)
	}
	if _, err := fixture.manager.InspectImage(fixture.ctx, missingAlias, ""); err != nil {
		t.Fatalf("idempotent retry did not restore a missing selected-image alias: %v", err)
	}
}

func (fixture *runtimeDataFixture) nativeResources(t *testing.T) {
	outside := "/tmp/" + fixture.prefix + "-outside.txt"
	fixture.backendPathCommand(t, "sh", "-c", `set -eu; printf 'synthetic outside boundary' > "$1"`, "porto-native-fixture", outside)
	fixture.t.Cleanup(func() { fixture.backendPathCommand(fixture.t, "rm", "--", outside) })
	volume := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "volume" })]
	descriptor, err := fixture.manager.FileDescriptor(fixture.ctx, "volume", volume.Destination)
	if err != nil {
		t.Fatal(err)
	}
	fixture.backendPathCommand(t, "sudo", "-n", "ln", "-s", "--", outside, path.Join(descriptor.RootPath, "escape-link"))
	for _, kind := range []string{"container", "image", "volume", "vm"} {
		t.Run(kind, func(t *testing.T) {
			name := fixture.instance.Name
			if kind != "vm" {
				index := slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == kind })
				name = fixture.selection[index].Destination
			}
			target, err := fixture.nativeTarget(kind, name, kind != "image")
			if err != nil {
				t.Fatal(err)
			}
			attachment, err := fixture.native.Attach(fixture.ctx, target, kind != "image")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := fixture.native.Detach(context.Background(), attachment.ID); err != nil {
					t.Error(err)
				}
			}()
			if kind == "image" {
				content, err := os.ReadFile(filepath.Join(attachment.Path, "synthetic.txt"))
				if err != nil || string(content) != "synthetic runtime file\n" {
					t.Fatalf("immutable image host path failed: %q, %v", content, err)
				}
				if err := os.WriteFile(filepath.Join(attachment.Path, "synthetic.txt"), []byte("forbidden"), 0o600); err == nil {
					t.Fatal("image accepted a host edit")
				}
				fixture.requireNativePaths(t, kind, name, attachment)
				return
			}
			path := filepath.Join(attachment.Path, "native-\u03b4.txt")
			content := []byte("synthetic native saved edit\n")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "vm" {
				output, err := fixture.vms.Exec(fixture.ctx, name, []string{"cat", "native-\u03b4.txt"}, nil)
				if err != nil || !reflect.DeepEqual(output, content) {
					t.Fatalf("managed VM edit was not direct: %q, %v", output, err)
				}
			} else {
				descriptor, err := fixture.manager.FileDescriptor(fixture.ctx, kind, name)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				if err := fixture.manager.FileRequest(fixture.ctx, kind, name, datafiles.Request{Action: "read", Path: "native-\u03b4.txt", Identity: descriptor.Resource.Fingerprint()}, nil, &output); err != nil {
					t.Fatal(err)
				}
				var file datafiles.Content
				if err := json.Unmarshal(output.Bytes(), &file); err != nil || file.Text != string(content) {
					t.Fatalf("workload-visible host edit failed: %s, %v", output.Bytes(), err)
				}
			}
			fixture.requireNativePaths(t, kind, name, attachment)
			if kind == "container" {
				if err := os.WriteFile(filepath.Join(attachment.Path, "readonly", "forbidden.txt"), []byte("forbidden"), 0o600); err == nil {
					t.Fatal("native container access bypassed a read-only volume mount")
				}
			}
		})
	}
}

func (fixture *runtimeDataFixture) nativeTarget(kind, name string, writable bool) (nativefiles.Target, error) {
	var target nativefiles.Target
	var err error
	if kind == "vm" {
		target, err = fixture.vms.NativeFilesTarget(fixture.ctx, name, writable)
	} else {
		target, err = fixture.manager.NativeFilesTarget(fixture.ctx, kind, name, writable)
	}
	if err != nil {
		return target, err
	}
	if target.SFTP != nil {
		if override := fixture.destinationHelper; override != "" && kind != "vm" {
			target.SFTP.Args = slices.Clone(target.SFTP.Args)
			for index, arg := range target.SFTP.Args {
				if strings.Contains(arg, `"$HOME/.local/bin/porto-runtime-helper" files-sftp`) {
					target.SFTP.Args[index] = strings.ReplaceAll(arg, `"$HOME/.local/bin/porto-runtime-helper"`, `"`+override+`"`)
				}
			}
		}
		if kind == "vm" && fixture.vmRunner.runAs != "" {
			command := fixture.vmRunner.command(runtimes.Command{Name: target.SFTP.Name, Args: target.SFTP.Args, Env: target.SFTP.Env})
			target.SFTP = &nativefiles.SFTPCommand{Name: command.Name, Args: command.Args, Env: command.Env}
		}
	}
	return target, nil
}
