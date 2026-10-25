//go:build windows

package windowscontainers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/moby/moby/api/pkg/stdcopy"
	dockerclient "github.com/moby/moby/client"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

// This test deliberately owns a real per-user service and Windows layers. It
// only runs on a disposable, elevated Server 2025 GitHub Actions worker. An
// opted-in supported row may fail, never turn a native failure into a skip.
func TestNativeWindowsContainersAcceptance(t *testing.T) {
	if os.Getenv("PORTO_TEST_NATIVE_WINDOWS_CONTAINERS") != "1" {
		t.Skip("set PORTO_TEST_NATIVE_WINDOWS_CONTAINERS=1 on a disposable Windows Actions worker")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Fatal("native Windows acceptance is restricted to disposable GitHub Actions workers")
	}
	if runtime.GOARCH != "amd64" || !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("the supported native row requires an elevated, real Windows AMD64 process, not emulation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	host, err := Probe(ctx)
	if err != nil || host.Validate() != nil || !host.Server || host.Build != 26100 {
		t.Fatalf("native acceptance requires verified Server 2025 Containers capability: %+v, %v", host, err)
	}
	binary := os.Getenv("PORTO_TEST_WINDOWS_BINARY")
	if binary == "" {
		binary = os.Getenv("PORTO_TEST_WINDOWS_PORTO")
	}
	bundle := os.Getenv(RuntimeDirectoryEnv)
	image := os.Getenv("PORTO_TEST_WINDOWS_IMAGE")
	if image == "" {
		image = os.Getenv("PORTO_TEST_NATIVE_WINDOWS_IMAGE")
	}
	wrongImage := os.Getenv("PORTO_TEST_NATIVE_WINDOWS_WRONG_IMAGE")
	if wrongImage == "" {
		wrongImage = nativeIncompatibleImage(t, ctx)
	}
	if binary == "" || bundle == "" || !strings.Contains(image, "@sha256:") || !strings.Contains(wrongImage, "@sha256:") {
		t.Fatal("provide current PORTO_TEST_WINDOWS_BINARY, PORTO_WINDOWS_RUNTIME_DIR, and pinned Server 2025 PORTO_TEST_WINDOWS_IMAGE")
	}
	if err := validateAMD64Executable(binary); err != nil {
		t.Fatal(err)
	}
	build, err := buildinfo.ReadFile(binary)
	if err != nil || build.Path != "github.com/mbianchidev/porto/cmd/porto" {
		t.Fatalf("acceptance must execute the current built Porto binary: %+v, %v", build, err)
	}
	for _, path := range runtimeSourcePaths(bundle) {
		if err := validateAMD64Executable(path); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(expected.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disposable native row already has runtime data; refusing to adopt or clean it: %v", err)
	}
	if service, err := openService(expected, true); !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		if service != nil {
			service.Close()
		}
		t.Fatalf("disposable row already has a native service: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(cwd, fmt.Sprintf(".porto-native-acceptance-%d", time.Now().UnixNano()))
	if err := os.Mkdir(fixture, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(fixture); err != nil {
			t.Errorf("remove owned source fixture: %v", err)
		}
	})
	t.Setenv("PORTO_HOME", filepath.Join(fixture, "ordinary-user"))
	t.Setenv("DOCKER_CONFIG", filepath.Join(fixture, "docker-client"))
	engineDir, err := config.DockerEngineDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(engineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linuxState := []byte(`{"mode":"lima","instance":"synthetic-linux-do-not-touch","ownerId":"synthetic-owner"}`)
	sentinel := filepath.Join(engineDir, "engine.json")
	if err := os.WriteFile(sentinel, linuxState, 0o600); err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) ([]byte, error) {
		cmd := process.NewCommand(ctx, "", binary, args...)
		return cmd.CombinedOutput()
	}
	action := func(name string) {
		t.Helper()
		args := []string{"docker", "windows", name}
		if name == "stop" || name == "remove" || name == "context" {
			args = append(args, "--confirm")
		}
		output, err := command(args...)
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(expected.Root, "service.log"))
			t.Fatalf("native %s: %v\n%s\nservice.log:\n%s", name, err, output, log)
		}
	}
	assertLinuxUnchanged := func() {
		t.Helper()
		actual, err := os.ReadFile(sentinel)
		if err != nil || !bytes.Equal(actual, linuxState) {
			t.Fatalf("native mode changed Linux store: %s, %v", actual, err)
		}
		if _, err := os.Stat(filepath.Join(fixture, "ordinary-user", "porto.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("elevated native action opened the ordinary project database: %v", err)
		}
	}
	// A collision must be detected before creating a directory or service, and
	// the foreign pipe must remain alive.
	foreign, err := winio.ListenPipe(expected.ContainerdPipe(), &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)",
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignDone := make(chan struct{})
	go func() {
		defer close(foreignDone)
		for {
			connection, err := foreign.Accept()
			if err != nil {
				return
			}
			connection.Close()
		}
	}()
	output, installErr := command("docker", "windows", "install")
	if installErr == nil || !strings.Contains(string(output), "ownership") {
		t.Fatalf("foreign containerd pipe was adopted: %v, %s", installErr, output)
	}
	if _, err := os.Stat(expected.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pipe collision left partial provisioning: %v", err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	<-foreignDone
	action("install")
	assertLinuxUnchanged()
	installed, err := readInstallation(expected)
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is bounded to the exact fixture whose absence was proved above.
	// No service/HNS search result or unrelated Windows/WSL store is removed.
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		if service, err := openService(expected, false); err == nil {
			current, readErr := readInstallation(expected)
			if readErr != nil || requireOwnedService(service, current) != nil {
				service.Close()
				t.Errorf("refusing cleanup of changed native ownership: %v", readErr)
				return
			}
			nativeClient := ClientFor(current)
			for _, id := range nativeFixtureContainers(cleanupContext, current) {
				request, _ := http.NewRequestWithContext(cleanupContext, http.MethodDelete, "http://localhost/containers/"+url.PathEscape(id)+"?force=true", nil)
				response, err := nativeClient.Do(request)
				if err != nil {
					nativeClient.CloseIdleConnections()
					service.Close()
					t.Errorf("remove only owned native fixture through its runtime API: %v", err)
					return
				}
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024))
				response.Body.Close()
				if readErr != nil || response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
					nativeClient.CloseIdleConnections()
					service.Close()
					t.Errorf("remove owned native fixture %s: HTTP %d, %s, %v", id, response.StatusCode, body, readErr)
					return
				}
			}
			nativeClient.CloseIdleConnections()
			if err := stopService(cleanupContext, service, current); err != nil {
				service.Close()
				t.Errorf("stop only owned fixture service: %v", err)
				return
			}
			if err := service.Delete(); err != nil {
				t.Errorf("remove owned fixture service: %v", err)
			}
			service.Close()
		} else if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			t.Errorf("inspect fixture cleanup: %v", err)
			return
		}
		ledger, err := networkLedger(installed)
		if err != nil {
			t.Errorf("read owned fixture HNS ledger: %v", err)
			return
		}
		networks, err := hcn.ListNetworks()
		if err != nil {
			t.Errorf("inspect owned fixture HNS networks: %v", err)
			return
		}
		endpoints, err := hcn.ListEndpoints()
		if err != nil {
			t.Errorf("inspect owned fixture endpoints: %v", err)
			return
		}
		for _, network := range networks {
			receipt, ok := ledger[network.Name]
			if !ok {
				continue
			}
			if err := checkNetwork(receipt, network); err != nil {
				t.Errorf("refuse changed fixture network: %v", err)
				return
			}
			for _, endpoint := range endpoints {
				if strings.EqualFold(endpoint.HostComputeNetwork, network.Id) {
					t.Errorf("owned fixture network still has endpoint %s; preserve data", endpoint.Id)
					return
				}
			}
			if err := network.Delete(); err != nil {
				t.Errorf("remove only owned fixture HNS network: %v", err)
				return
			}
		}
		if err := protectedTree(expected.Root); err != nil {
			t.Errorf("refuse changed fixture root: %v", err)
			return
		}
		layers := filepath.Join(installed.Data(), "containerd", "io.containerd.snapshotter.v1.windows", "snapshots")
		entries, err := os.ReadDir(layers)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("inspect owned fixture Windows layers: %v", err)
			return
		}
		if err := winio.RunWithPrivileges([]string{winio.SeBackupPrivilege, winio.SeRestorePrivilege}, func() error {
			for _, entry := range entries {
				if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("refuse unexpected fixture layer entry %s", entry.Name())
				}
				if err := hcsshim.DestroyLayer(hcsshim.DriverInfo{HomeDir: layers}, entry.Name()); err != nil {
					return fmt.Errorf("destroy owned fixture Windows layer %s: %w", entry.Name(), err)
				}
			}
			return nil
		}); err != nil {
			t.Errorf("remove owned fixture Windows layers through HCS: %v", err)
			return
		}
		if err := os.RemoveAll(expected.Root); err != nil {
			t.Errorf("remove owned disposable native runtime fixture: %v", err)
		}
	})
	verifyInstalledFile := func(source, relative string) {
		t.Helper()
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if installed.Hashes[relative] != hex.EncodeToString(hash[:]) {
			t.Fatalf("native runtime used a stale binary instead of current %s", source)
		}
	}
	verifyInstalledFile(binary, "porto.exe")
	for _, file := range runtimeFiles {
		verifyInstalledFile(filepath.Join(bundle, file), filepath.Join("bin", file))
	}
	status := Snapshot(ctx)
	if !status.Supported || !status.Installed || !status.Owned || !status.Running ||
		status.Context != ContextName || !status.Capabilities.Process.Supported {
		t.Fatalf("installed native status = %+v", status)
	}
	client := ClientFor(expected)
	defer client.CloseIdleConnections()
	request := func(method, path string, body any, want int) []byte {
		t.Helper()
		var reader io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			reader = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reader)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("native API %s %s: %v", method, path, err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024))
		if err != nil || response.StatusCode != want {
			t.Fatalf("native API %s %s = %d: %s, %v", method, path, response.StatusCode, data, err)
		}
		return data
	}
	version := request("GET", "/version", nil, http.StatusOK)
	if !bytes.Contains(version, []byte(`"Os":"windows"`)) {
		t.Fatalf("native endpoint advertises the wrong OS: %s", version)
	}
	anonymousPipeDenied(t, installed.Pipe())
	anonymousPipeDenied(t, installed.ContainerdPipe())
	testNativeWindowsNetworking(t, ctx, installed)
	createdNetwork := request("POST", "/networks/create", map[string]any{
		"Name": "synthetic-native-network", "Driver": "nat",
		"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": "10.252.239.0/24"}}}}, http.StatusCreated)
	var network struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(createdNetwork, &network); err != nil || network.ID == "" {
		t.Fatalf("native network identity: %s, %v", createdNetwork, err)
	}
	request("GET", "/networks/"+network.ID, nil, http.StatusOK)
	request("DELETE", "/networks/"+network.ID, nil, http.StatusNoContent)
	request("POST", "/images/create?"+url.Values{"fromImage": {wrongImage}}.Encode(), nil, http.StatusBadRequest)
	request("POST", "/images/create?"+url.Values{"fromImage": {image}, "platform": {"windows/arm64"}}.Encode(), nil, http.StatusBadRequest)
	request("POST", "/images/create?"+url.Values{"fromImage": {image}}.Encode(), nil, http.StatusOK)
	imageDocument := request("GET", "/images/"+url.PathEscape(image)+"/json", nil, http.StatusOK)
	var imageIdentity struct {
		ID         string `json:"Id"`
		Descriptor struct{ Digest string }
	}
	if err := json.Unmarshal(imageDocument, &imageIdentity); err != nil || imageIdentity.ID == "" ||
		imageIdentity.Descriptor.Digest == "" || imageIdentity.ID == imageIdentity.Descriptor.Digest {
		t.Fatalf("native image config ID and manifest target were conflated: %s, %v", imageDocument, err)
	}
	if listed := request("GET", "/images/json", nil, http.StatusOK); !bytes.Contains(listed, []byte(imageIdentity.ID)) {
		t.Fatalf("native image listing lost Docker configuration identity: %s", listed)
	}
	alias := "registry.example.test/synthetic-native:fixture"
	request("POST", "/images/"+url.PathEscape(imageIdentity.ID)+"/tag?repo=registry.example.test%2Fsynthetic-native&tag=fixture", nil, http.StatusCreated)
	request("GET", "/images/"+url.PathEscape(alias)+"/json", nil, http.StatusOK)
	request("DELETE", "/images/"+url.PathEscape(alias), nil, http.StatusOK)
	request("POST", "/containers/create", map[string]any{"Image": image, "HostConfig": map[string]any{"Isolation": "host"}}, http.StatusBadRequest)
	bind := filepath.Join(fixture, "quote's é directory")
	if err := os.Mkdir(bind, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bind, "host.txt"), []byte("host-to-native-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixtureBuild := process.NewCommand(ctx, "", "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
		"-o", filepath.Join(bind, "native-fixture.exe"), "./testfixture")
	fixtureBuild.Env = process.WithEnvironment(os.Environ(), "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64")
	if output, err := fixtureBuild.CombinedOutput(); err != nil {
		t.Fatalf("build current native HTTP/exec fixture: %v\n%s", err, output)
	}
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	portListener.Close()
	name := "porto-native-acceptance"
	body := map[string]any{"Image": imageIdentity.ID, "User": "ContainerAdministrator", "Cmd": []string{`C:\fixture\native-fixture.exe`},
		"Labels": map[string]string{"io.porto.test.native": "true"},
		"HostConfig": map[string]any{"Isolation": "process",
			"Mounts":       []map[string]any{{"Type": "bind", "Source": bind, "Target": `C:\fixture`}},
			"PortBindings": map[string]any{"8180/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": fmt.Sprint(port)}}}}}
	created := request("POST", "/containers/create?name="+name, body, http.StatusCreated)
	var container struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(created, &container); err != nil || container.ID == "" {
		t.Fatalf("native create identity: %s, %v", created, err)
	}
	beforeStart := request("GET", "/containers/"+container.ID+"/json", nil, http.StatusOK)
	var createdState struct {
		State struct {
			Status  string
			Running bool
			Pid     uint32
		}
	}
	if err := json.Unmarshal(beforeStart, &createdState); err != nil || createdState.State.Status != "created" ||
		createdState.State.Running || createdState.State.Pid != 0 {
		t.Fatalf("native Docker create executed or started a workload: %s, %v", beforeStart, err)
	}
	if _, err := os.Stat(filepath.Join(bind, "container.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native fixture ran before explicit /start: %v", err)
	}
	request("POST", "/containers/"+container.ID+"/start", nil, http.StatusNoContent)
	waitNativeHTTP(t, ctx, port, "host-to-native-one")
	inspect := request("GET", "/containers/"+container.ID+"/json", nil, http.StatusOK)
	if !bytes.Contains(inspect, []byte(container.ID)) {
		t.Fatalf("native inspect lost container identity: %s", inspect)
	}
	content, err := os.ReadFile(filepath.Join(bind, "container.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "native-to-host-one" {
		t.Fatalf("native-to-host bind write: %s, %v", content, err)
	}
	logs := request("GET", "/containers/"+container.ID+"/logs?stdout=1&stderr=0&tail=100", nil, http.StatusOK)
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, bytes.NewReader(logs)); err != nil || !strings.Contains(stdout.String(), "porto-native-ready") {
		t.Fatalf("native stdout logs = %s, %s, %v", stdout.String(), stderr.String(), err)
	}
	docker, err := dockerclient.New(dockerclient.WithHost(installed.Endpoint()), dockerclient.WithHTTPClient(ClientFor(expected)),
		dockerclient.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	execResult, err := docker.ExecCreate(ctx, container.ID, dockerclient.ExecCreateOptions{AttachStdout: true, AttachStderr: true,
		Cmd: []string{`C:\fixture\native-fixture.exe`, "exec"}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := docker.ExecAttach(ctx, execResult.ID, dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	_, copyErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
	attached.Close()
	if copyErr != nil || !strings.Contains(stdout.String(), "host-to-native-one") || !strings.Contains(stderr.String(), "porto-native-exec-stderr") {
		t.Fatalf("native exec stdout=%s stderr=%s: %v", stdout.String(), stderr.String(), copyErr)
	}
	executed, err := docker.ExecInspect(ctx, execResult.ID, dockerclient.ExecInspectOptions{})
	if err != nil || executed.ExitCode != 0 {
		t.Fatalf("native exec exit=%+v, %v", executed, err)
	}
	failed, err := docker.ExecCreate(ctx, container.ID, dockerclient.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: []string{`C:\fixture\native-fixture.exe`, "fail"}})
	if err != nil {
		t.Fatal(err)
	}
	failedAttach, err := docker.ExecAttach(ctx, failed.ID, dockerclient.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, failCopyErr := stdcopy.StdCopy(io.Discard, io.Discard, failedAttach.Reader)
	failedAttach.Close()
	failState, err := docker.ExecInspect(ctx, failed.ID, dockerclient.ExecInspectOptions{})
	if failCopyErr != nil || err != nil || failState.ExitCode != 37 {
		t.Fatalf("native exec lost nonzero exit code: %+v, %v, %v", failState, err, failCopyErr)
	}
	runNativeTTYExec(t, ctx, docker, container.ID)
	t.Run("dashboard-WebSocket", func(t *testing.T) {
		runNativeDashboardWebSocket(t, ctx, installed, docker, container.ID)
	})
	content, err = os.ReadFile(filepath.Join(bind, "exec.txt"))
	if err != nil || string(content) != "native-exec-write" {
		t.Fatalf("native exec bind write = %s, %v", content, err)
	}
	ttyCreated := request("POST", "/containers/create?name=porto-native-tty", map[string]any{
		"Image": image, "User": "ContainerAdministrator", "Cmd": []string{`C:\fixture\native-fixture.exe`, "tty"},
		"Tty": true, "OpenStdin": true, "Labels": map[string]string{"io.porto.test.native": "true"},
		"HostConfig": map[string]any{"Isolation": "process", "ConsoleSize": [2]uint{30, 90},
			"Mounts": []map[string]any{{"Type": "bind", "Source": bind, "Target": `C:\fixture`}}}}, http.StatusCreated)
	var ttyContainer struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(ttyCreated, &ttyContainer); err != nil || ttyContainer.ID == "" {
		t.Fatalf("native TTY container create: %s, %v", ttyCreated, err)
	}
	request("POST", "/containers/"+ttyContainer.ID+"/start", nil, http.StatusNoContent)
	runNativeTTYAttach(t, ctx, docker, ttyContainer.ID)
	request("DELETE", "/containers/"+ttyContainer.ID, nil, http.StatusNoContent)
	for _, action := range []string{"stop", "remove", "install"} {
		args := []string{"docker", "windows", action}
		if action != "install" {
			args = append(args, "--confirm")
		}
		output, err := command(args...)
		if err == nil || !strings.Contains(string(output), "active") {
			t.Fatalf("native %s interrupted an active workload: %v, %s", action, err, output)
		}
	}
	ownedService, err := openService(installed, false)
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := ownedService.Config()
	if err != nil {
		ownedService.Close()
		t.Fatal(err)
	}
	foreignConfiguration := configuration
	foreignConfiguration.Description = "synthetic foreign ownership marker"
	if err := ownedService.UpdateConfig(foreignConfiguration); err != nil {
		ownedService.Close()
		t.Fatal(err)
	}
	output, ownershipErr := command("docker", "windows", "remove", "--confirm")
	restoreErr := ownedService.UpdateConfig(configuration)
	state, stateErr := ownedService.Query()
	ownedService.Close()
	if restoreErr != nil || stateErr != nil || ownershipErr == nil || !strings.Contains(string(output), "ownership") || state.State != svc.Running {
		t.Fatalf("ownership denial failed: err=%v output=%s restore=%v state=%+v %v", ownershipErr, output, restoreErr, state, stateErr)
	}
	request("POST", "/containers/"+container.ID+"/stop?t=10", nil, http.StatusNoContent)
	client.CloseIdleConnections()
	action("stop")
	assertLinuxUnchanged()
	action("start")
	request("GET", "/containers/"+container.ID+"/json", nil, http.StatusOK)
	request("POST", "/containers/"+container.ID+"/start", nil, http.StatusNoContent)
	if err := os.WriteFile(filepath.Join(bind, "host.txt"), []byte("host-to-native-after-restart"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitNativeHTTP(t, ctx, port, "host-to-native-after-restart")
	request("POST", "/containers/"+container.ID+"/stop?t=10", nil, http.StatusNoContent)
	client.CloseIdleConnections()
	action("remove")
	if _, err := os.Stat(filepath.Join(installed.Data(), "containerd")); err != nil {
		t.Fatalf("native removal discarded data: %v", err)
	}
	action("install")
	assertLinuxUnchanged()
	request("GET", "/containers/"+container.ID+"/json", nil, http.StatusOK)
	request("GET", "/images/"+url.PathEscape(image)+"/json", nil, http.StatusOK)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("native context acceptance requires the current bundled Docker CLI on PATH")
	}
	action("context")
	metadata, err := process.NewCommand(ctx, "", "docker", "context", "inspect", ContextName).CombinedOutput()
	if err != nil || !strings.Contains(string(metadata), installed.Endpoint()) {
		t.Fatalf("distinct native context = %s, %v", metadata, err)
	}
	request("DELETE", "/containers/"+container.ID, nil, http.StatusNoContent)
	request("DELETE", "/images/"+url.PathEscape(imageIdentity.ID)+"?force=true", nil, http.StatusOK)
	assertLinuxUnchanged()
}

func nativeTTYChunks(reader io.Reader) <-chan string {
	chunks := make(chan string, 64)
	go func() {
		defer close(chunks)
		data := make([]byte, 4096)
		for {
			n, err := reader.Read(data)
			if n > 0 {
				chunks <- string(data[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}

func nativeTTYExpect(t *testing.T, ctx context.Context, chunks <-chan string, want string) {
	t.Helper()
	var output strings.Builder
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("native TTY closed before %q: %s", want, output.String())
			}
			output.WriteString(chunk)
			if strings.Contains(output.String(), want) {
				return
			}
		case <-ctx.Done():
			t.Fatalf("native TTY did not produce %q: %s", want, output.String())
		}
	}
}

func nativeTTYFinish(t *testing.T, ctx context.Context, chunks <-chan string) {
	t.Helper()
	for {
		select {
		case _, ok := <-chunks:
			if !ok {
				return
			}
		case <-ctx.Done():
			t.Fatal("native TTY did not close and reap its client")
		}
	}
}

func runNativeTTYExec(t *testing.T, parent context.Context, client *dockerclient.Client, containerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	exec, err := client.ExecCreate(ctx, containerID, dockerclient.ExecCreateOptions{
		TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, ConsoleSize: dockerclient.ConsoleSize{Width: 90, Height: 30},
		Cmd: []string{`C:\fixture\native-fixture.exe`, "tty"}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.ExecAttach(ctx, exec.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunks := nativeTTYChunks(stream.Reader)
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-ready")
	if _, err := stream.Conn.Write([]byte("wait-size 90 30\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-size:90x30")
	if _, err := client.ExecResize(ctx, exec.ID, dockerclient.ExecResizeOptions{Width: 132, Height: 41}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Conn.Write([]byte("wait-size 132 41\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-size:132x41")
	if _, err := stream.Conn.Write([]byte("quit\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-final-output")
	nativeTTYFinish(t, ctx, chunks)
	result, err := client.ExecInspect(ctx, exec.ID, dockerclient.ExecInspectOptions{})
	if err != nil || result.Running || result.ExitCode != 37 {
		t.Fatalf("native TTY exec status=%+v, %v", result, err)
	}
}

func runNativeTTYAttach(t *testing.T, parent context.Context, client *dockerclient.Client, containerID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	stream, err := client.ContainerAttach(ctx, containerID, dockerclient.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunks := nativeTTYChunks(stream.Reader)
	if _, err := stream.Conn.Write([]byte("wait-size 90 30\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-size:90x30")
	if _, err := client.ContainerResize(ctx, containerID, dockerclient.ContainerResizeOptions{Width: 133, Height: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Conn.Write([]byte("wait-size 133 42\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-size:133x42")
	if _, err := stream.Conn.Write([]byte("quit\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, chunks, "porto-native-tty-final-output")
	nativeTTYFinish(t, ctx, chunks)
	wait := client.ContainerWait(ctx, containerID, dockerclient.ContainerWaitOptions{})
	results, failures := wait.Result, wait.Error
	for {
		select {
		case result, ok := <-results:
			if !ok || result.StatusCode != 37 {
				t.Fatalf("native TTY attached container status=%+v, available=%v", result, ok)
			}
			return
		case err, ok := <-failures:
			if ok && err != nil {
				t.Fatal(err)
			}
			failures = nil
		case <-ctx.Done():
			t.Fatal("native TTY attached container wait did not complete")
		}
	}
}

func nativeIncompatibleImage(t *testing.T, ctx context.Context) string {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, "GET", "https://mcr.microsoft.com/v2/windows/nanoserver/manifests/ltsc2022", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	client := &http.Client{Timeout: 45 * time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("resolve incompatible native image read-only: %v", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024+1))
	if err != nil || response.StatusCode != http.StatusOK || len(data) > 4*1024*1024 {
		t.Fatalf("resolve incompatible image metadata: HTTP %d, %v", response.StatusCode, err)
	}
	hash := sha256.Sum256(data)
	pinned := "sha256:" + hex.EncodeToString(hash[:])
	if declared := response.Header.Get("Docker-Content-Digest"); declared != "" && declared != pinned {
		t.Fatalf("incompatible registry metadata digest differs: %s instead of %s", declared, pinned)
	}
	return "mcr.microsoft.com/windows/nanoserver@" + pinned
}

func waitNativeHTTP(t *testing.T, ctx context.Context, port int, want string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/", port), nil)
		response, err := client.Do(request)
		if err == nil {
			content, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
			response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK && string(content) == want {
				return
			}
			err = fmt.Errorf("HTTP %d body=%q: %v", response.StatusCode, content, readErr)
		}
		last = err
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("native localhost NAT/bind response never became %q: %v", want, last)
}

func anonymousPipeDenied(t *testing.T, path string) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	procedure := windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateAnonymousToken")
	runtime.LockOSThread()
	result, _, callErr := procedure.Call(uintptr(windows.CurrentThread()))
	if result == 0 {
		runtime.UnlockOSThread()
		t.Fatalf("create anonymous permission probe: %v", callErr)
	}
	handle, accessErr := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if accessErr == nil {
		windows.CloseHandle(handle)
	}
	revertErr := windows.RevertToSelf()
	runtime.UnlockOSThread()
	if revertErr != nil {
		t.Fatal(revertErr)
	}
	if !errors.Is(accessErr, windows.ERROR_ACCESS_DENIED) && !errors.Is(accessErr, windows.ERROR_BAD_IMPERSONATION_LEVEL) && !errors.Is(accessErr, windows.ERROR_CANT_OPEN_ANONYMOUS) {
		t.Fatalf("unrelated anonymous token was not denied native pipe %s: %v", path, accessErr)
	}
}

func nativeFixtureContainers(ctx context.Context, i Installation) []string {
	output, err := runNerdctl(ctx, i, "ps", "-a", "--quiet", "--filter", "label=io.porto.test.native=true")
	if err != nil {
		return nil
	}
	return strings.Fields(string(output))
}
