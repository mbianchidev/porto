//go:build windows

package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/pe"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/hcn"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func coexistenceFileHash(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func coexistenceServiceAbsent(installation windowscontainers.Installation) error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(installation.ServiceName())
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if service != nil {
		service.Close()
	}
	if err != nil {
		return err
	}
	return errors.New("native service already exists")
}

func removeCoexistenceNetwork(installation windowscontainers.Installation) error {
	if err := windowscontainers.CheckNetworks(installation); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(installation.Root, "networks.json"))
	if err != nil {
		return err
	}
	var ledger map[string]windowscontainers.Network
	if err := json.Unmarshal(data, &ledger); err != nil {
		return err
	}
	receipt, ok := ledger[installation.DefaultNetwork()]
	if !ok || len(ledger) != 1 || receipt.Name != installation.DefaultNetwork() || receipt.ID == "" {
		return errors.New("refuse cleanup of unexpected native coexistence network receipts")
	}
	networks, err := hcn.ListNetworks()
	if err != nil {
		return err
	}
	endpoints, err := hcn.ListEndpoints()
	if err != nil {
		return err
	}
	for _, network := range networks {
		if network.Name != receipt.Name && !strings.EqualFold(network.Id, receipt.ID) {
			continue
		}
		if network.Name != receipt.Name || !strings.EqualFold(network.Id, receipt.ID) || network.Type != hcn.NAT ||
			len(network.Ipams) != 1 || len(network.Ipams[0].Subnets) != 1 ||
			network.Ipams[0].Subnets[0].IpAddressPrefix != receipt.Subnet {
			return errors.New("refuse cleanup of a replaced native coexistence HNS network")
		}
		for _, endpoint := range endpoints {
			if strings.EqualFold(endpoint.HostComputeNetwork, network.Id) {
				return errors.New("refuse cleanup of native coexistence network with an unexpected live endpoint")
			}
		}
		if err := network.Delete(); err != nil {
			return fmt.Errorf("remove only receipted native coexistence HNS network: %w", err)
		}
	}
	return nil
}

func testWindowsLinuxCoexistence(t *testing.T, parent context.Context, manager *Manager, linuxHost, image, suite, workspace string) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" || runtime.GOARCH != "amd64" || !windows.GetCurrentProcessToken().IsElevated() ||
		os.Getenv("PORTO_DOCKER_CLIENT_BACKEND") != "lima" || os.Getenv("PORTO_DOCKER_CLIENT_OWN_FRESH_LIMA") != "1" ||
		manager.IsNativeWindows() {
		t.Fatal("live coexistence requires an elevated, disposable Windows AMD64 worker and its independently owned fresh Linux Lima engine")
	}
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	host, err := windowscontainers.Probe(ctx)
	if err != nil || host.Validate() != nil || !host.Server || host.Build != 26100 {
		t.Fatalf("live coexistence requires real Server 2025 Containers capability: %+v, %v", host, err)
	}
	ownership := manager.EngineOwnershipStatus(ctx)
	if !ownership.Owned || !ownership.Verified {
		t.Fatalf("refusing an unverified Linux fixture: %+v", ownership)
	}
	source := os.Getenv("PORTO_TEST_WINDOWS_BINARY")
	if source == "" {
		source = os.Getenv("PORTO_TEST_PORTO_BINARY")
	}
	bundle := os.Getenv(windowscontainers.RuntimeDirectoryEnv)
	if !filepath.IsAbs(source) || !filepath.IsAbs(bundle) {
		t.Fatal("live coexistence requires absolute current PORTO_TEST_WINDOWS_BINARY (or PORTO_TEST_PORTO_BINARY) and PORTO_WINDOWS_RUNTIME_DIR")
	}
	build, err := buildinfo.ReadFile(source)
	if err != nil || build.Path != "github.com/mbianchidev/porto/cmd/porto" {
		t.Fatalf("coexistence must execute the current Porto CLI, not a test substitute: %+v, %v", build, err)
	}
	runtimeFiles := []string{"containerd.exe", "containerd-shim-runhcs-v1.exe", "nerdctl.exe", "nat.exe", "sdnbridge.exe", "sdnoverlay.exe"}
	payloadPaths := []string{source}
	for _, name := range runtimeFiles {
		payloadPaths = append(payloadPaths, filepath.Join(bundle, name))
	}
	for _, file := range payloadPaths {
		executable, err := pe.Open(file)
		if err != nil {
			t.Fatalf("inspect native fixture payload %s: %v", file, err)
		}
		machine := executable.Machine
		executable.Close()
		if machine != pe.IMAGE_FILE_MACHINE_AMD64 {
			t.Fatalf("native coexistence payload is not real AMD64: %s", file)
		}
	}
	expected, err := windowscontainers.Current()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(expected.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native coexistence refuses pre-existing runtime data: %v", err)
	}
	if err := coexistenceServiceAbsent(expected); err != nil {
		t.Fatalf("native coexistence refuses a pre-existing service: %v", err)
	}

	linux := dockerSDKClient(t, linuxHost, dockerclient.WithAPIVersion("1.47"))
	volumeName, containerName := suite+"-coexistence-data", suite+"-coexistence"
	if _, err := manager.CreateVolume(ctx, volumeName, "local", map[string]string{"porto.test.suite": suite}); err != nil {
		t.Fatalf("create real Linux coexistence volume: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := manager.RemoveVolume(cleanup, volumeName, false); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove only owned Linux coexistence volume: %v", err)
		}
	})
	workload, err := linux.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: containerName,
		Config: &container.Config{Image: image, Labels: map[string]string{"porto.test.suite": suite},
			Cmd: []string{"sh", "-c", `set -eu
printf '%s\n' "$1" > /coexistence/identity
: > /coexistence/journal
while :; do
  if [ -f /coexistence/request ]; then
    cat /coexistence/request > /coexistence/.observed
    mv /coexistence/.observed /coexistence/observed
  fi
  sleep 0.1
done`, "porto-coexistence", suite}},
		HostConfig: &container.HostConfig{NetworkMode: "none",
			Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/coexistence"}}},
	})
	if err != nil {
		t.Fatalf("create real Linux coexistence workload: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if err := manager.ContainerAction(cleanup, workload.ID, "remove-force"); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove only owned Linux coexistence workload: %v", err)
		}
	})
	if _, err := linux.ContainerStart(ctx, workload.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("start real Linux coexistence workload: %v", err)
	}
	execLinux := func(arguments ...string) []byte {
		t.Helper()
		execution, err := linux.ExecCreate(ctx, workload.ID, dockerclient.ExecCreateOptions{
			Cmd: arguments, AttachStdout: true, AttachStderr: true,
		})
		if err != nil {
			t.Fatalf("create Linux coexistence exec: %v", err)
		}
		attached, err := linux.ExecAttach(ctx, execution.ID, dockerclient.ExecAttachOptions{})
		if err != nil {
			t.Fatalf("attach Linux coexistence exec: %v", err)
		}
		var stdout, stderr bytes.Buffer
		_, copyErr := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
		attached.Close()
		state, inspectErr := linux.ExecInspect(ctx, execution.ID, dockerclient.ExecInspectOptions{})
		if copyErr != nil || inspectErr != nil || state.Running || state.ExitCode != 0 || stderr.Len() != 0 {
			t.Fatalf("Linux coexistence exec %q: stdout=%q stderr=%q state=%+v copy=%v inspect=%v",
				arguments, stdout.Bytes(), stderr.Bytes(), state, copyErr, inspectErr)
		}
		return stdout.Bytes()
	}
	before, err := linux.ContainerInspect(ctx, workload.ID, dockerclient.ContainerInspectOptions{})
	if err != nil || before.Container.State == nil || !before.Container.State.Running || before.Container.State.Pid == 0 ||
		before.Container.State.StartedAt == "" || before.Container.ID != workload.ID {
		t.Fatalf("Linux coexistence workload did not actually start: %+v, %v", before, err)
	}
	readyDeadline := time.Now().Add(15 * time.Second)
	for {
		identity := execLinux("sh", "-c", `if [ -f /coexistence/identity ] && [ -f /coexistence/journal ]; then cat /coexistence/identity; fi`)
		if string(identity) == suite+"\n" {
			break
		}
		if time.Now().After(readyDeadline) {
			t.Fatalf("original Linux workload did not initialize its real volume: %q", identity)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	payload := bytes.Repeat([]byte("Porto synthetic Linux dataset \x00\xff\xfe \u03b4 \u00e9\n"), 128)
	execLinux("sh", "-c", `printf '%s' "$1" | base64 -d > /coexistence/dataset`, "porto-coexistence", base64.StdEncoding.EncodeToString(payload))
	var journal []byte
	sequence := 0
	assertLinuxLive := func(phase string) {
		t.Helper()
		after, err := linux.ContainerInspect(ctx, workload.ID, dockerclient.ContainerInspectOptions{})
		if err != nil || after.Container.State == nil || !after.Container.State.Running ||
			after.Container.State.Pid != before.Container.State.Pid || after.Container.State.StartedAt != before.Container.State.StartedAt ||
			after.Container.ID != workload.ID || after.Container.Image != before.Container.Image ||
			!reflect.DeepEqual(after.Container.Mounts, before.Container.Mounts) {
			t.Fatalf("%s changed or restarted the live Linux workload: before=%+v after=%+v, %v", phase, before, after, err)
		}
		if actual := execLinux("cat", "/coexistence/dataset"); !bytes.Equal(actual, payload) {
			t.Fatalf("%s changed real Linux volume bytes: got %d, want %d bytes", phase, len(actual), len(payload))
		}
		if actual := execLinux("cat", "/coexistence/journal"); !bytes.Equal(actual, journal) {
			t.Fatalf("%s changed Linux workload writes: got=%q want=%q", phase, actual, journal)
		}
		if identity := execLinux("cat", "/coexistence/identity"); string(identity) != suite+"\n" {
			t.Fatalf("%s replaced the Linux workload dataset identity: %q", phase, identity)
		}
		sequence++
		marker := fmt.Sprintf("%s/%02d/%s", suite, sequence, phase)
		execLinux("sh", "-c", `printf '%s\n' "$1" > /coexistence/request`, "porto-coexistence", marker)
		deadline := time.Now().Add(15 * time.Second)
		for {
			observed := execLinux("sh", "-c", `if [ -f /coexistence/observed ]; then cat /coexistence/observed; fi`)
			if string(observed) == marker+"\n" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s left Linux metadata present but the original live process stopped reading/writing: %q", phase, observed)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		execLinux("sh", "-c", `printf '%s\n' "$1" >> /coexistence/journal`, "porto-coexistence", marker)
		journal = append(journal, marker...)
		journal = append(journal, '\n')
		if actual := execLinux("cat", "/coexistence/journal"); !bytes.Equal(actual, journal) {
			t.Fatalf("%s lost a new Linux volume write: %q", phase, actual)
		}
		t.Logf("%s preserved Linux container %s PID %d, start %s and %d exact dataset bytes with live bidirectional I/O",
			phase, workload.ID, before.Container.State.Pid, before.Container.State.StartedAt, len(payload))
	}
	assertLinuxLive("before-native-install")

	dockerConfig := filepath.Join(workspace, "native-coexistence-docker")
	if err := os.Mkdir(dockerConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	environment := process.WithEnvironment(os.Environ(), "DOCKER_CONFIG="+dockerConfig, "DOCKER_HOST=", "DOCKER_CONTEXT=",
		windowscontainers.RuntimeDirectoryEnv+"="+bundle)
	command := func(commandContext context.Context, executable string, args ...string) ([]byte, error) {
		cmd := process.NewCommand(commandContext, "", executable, args...)
		cmd.Env = environment
		return cmd.CombinedOutput()
	}
	mustCommand := func(executable string, args ...string) []byte {
		t.Helper()
		commandContext, stop := context.WithTimeout(ctx, 90*time.Second)
		defer stop()
		output, err := command(commandContext, executable, args...)
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(expected.Root, "service.log"))
			t.Fatalf("%s %v: %v\n%s\nnative service.log:\n%s", executable, args, err, output, log)
		}
		return output
	}
	nativeAction := func(binary, action string) {
		t.Helper()
		args := []string{"docker", "windows", action}
		if action == "stop" || action == "remove" || action == "context" {
			args = append(args, "--confirm")
		}
		mustCommand(binary, args...)
	}
	var known windowscontainers.Installation
	var knownReceipt []byte
	recordInstallation := func(binary string, running bool) windowscontainers.Installation {
		t.Helper()
		status := windowscontainers.Snapshot(ctx)
		if !status.Installed || !status.Owned || status.Running != running || !status.Supported {
			t.Fatalf("native coexistence installation not verified: %+v", status)
		}
		data, err := os.ReadFile(filepath.Join(expected.Root, "installation.json"))
		if err != nil {
			t.Fatal(err)
		}
		var installed windowscontainers.Installation
		if err := json.Unmarshal(data, &installed); err != nil || installed.Version != 1 ||
			installed.Root != expected.Root || installed.OwnerSID != expected.OwnerSID || len(installed.Generation) != 32 ||
			strings.Trim(installed.Generation, "0123456789abcdef") != "" {
			t.Fatalf("native coexistence receipt has a changed identity: %+v, %v", installed, err)
		}
		known, knownReceipt = installed, append([]byte(nil), data...)
		hash := coexistenceFileHash(t, binary)
		if installed.Hashes["porto.exe"] != hash || coexistenceFileHash(t, installed.Binary()) != hash {
			t.Fatal("native deployment does not contain the independently built, expected current CLI")
		}
		for _, name := range runtimeFiles {
			if installed.Hashes[filepath.Join("bin", name)] != coexistenceFileHash(t, filepath.Join(bundle, name)) {
				t.Fatalf("native deployment used a stale or foreign payload: %s", name)
			}
		}
		return installed
	}
	cleanupNative := func(cleanup context.Context) error {
		if known.Generation == "" {
			return nil
		}
		current, err := os.ReadFile(filepath.Join(expected.Root, "installation.json"))
		if errors.Is(err, os.ErrNotExist) {
			return coexistenceServiceAbsent(expected)
		}
		if err != nil {
			return fmt.Errorf("read native fixture receipt before cleanup: %w", err)
		}
		if !bytes.Equal(current, knownReceipt) {
			return errors.New("refuse cleanup of a changed native fixture receipt")
		}
		status := windowscontainers.Snapshot(cleanup)
		if status.Installed {
			if !status.Owned {
				return fmt.Errorf("refuse cleanup of changed native service ownership: %s", status.Message)
			}
			if status.Running {
				native := windowscontainers.ClientFor(known)
				defer native.CloseIdleConnections()
				for _, resource := range []string{"/containers/json?all=1", "/images/json"} {
					request, err := http.NewRequestWithContext(cleanup, http.MethodGet, "http://localhost"+resource, nil)
					if err != nil {
						return err
					}
					response, err := native.Do(request)
					if err != nil {
						return err
					}
					var objects []json.RawMessage
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&objects)
					response.Body.Close()
					if decodeErr != nil || response.StatusCode != http.StatusOK || len(objects) != 0 {
						return fmt.Errorf("refuse cleanup of native fixture with unexpected %s: HTTP %d, objects=%d, %v", resource, response.StatusCode, len(objects), decodeErr)
					}
				}
			}
			output, err := command(cleanup, source, "docker", "windows", "remove", "--confirm")
			if err != nil {
				return fmt.Errorf("remove only owned native coexistence service: %w: %s", err, output)
			}
		}
		if err := coexistenceServiceAbsent(expected); err != nil {
			return err
		}
		if err := filepath.WalkDir(expected.Root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name, err := windows.UTF16PtrFromString(path)
			if err != nil {
				return err
			}
			attributes, err := windows.GetFileAttributes(name)
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return errors.New("refuse cleanup of a replaced native fixture path")
			}
			return nil
		}); err != nil {
			return err
		}
		if current, err := os.ReadFile(filepath.Join(expected.Root, "installation.json")); err != nil || !bytes.Equal(current, knownReceipt) {
			return errors.New("native fixture identity changed before deleting its proven-owned empty store")
		}
		if err := removeCoexistenceNetwork(known); err != nil {
			return err
		}
		if err := os.RemoveAll(expected.Root); err != nil {
			return err
		}
		known = windowscontainers.Installation{}
		return nil
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Minute)
		defer stop()
		if err := cleanupNative(cleanup); err != nil {
			t.Errorf("preserve unverified native coexistence fixture rather than delete it: %v", err)
		}
	})

	linuxContext := suite + "-linux"
	mustCommand("docker", "context", "create", linuxContext, "--docker", "host="+linuxHost)
	selectMode := func(native, available bool) {
		t.Helper()
		contextName, wantOS := linuxContext, "linux"
		if native {
			contextName, wantOS = windowscontainers.ContextName, "windows"
		}
		mustCommand("docker", "context", "use", contextName)
		if current := strings.TrimSpace(string(mustCommand("docker", "context", "show"))); current != contextName {
			t.Fatalf("container mode selection = %q, want %q", current, contextName)
		}
		modeContext, stop := context.WithTimeout(ctx, 15*time.Second)
		defer stop()
		info, err := command(modeContext, "docker", "info", "--format", "{{.OSType}}")
		if !available {
			if err == nil {
				t.Fatalf("unavailable native mode fell back to a live Linux backend: %s", info)
			}
			return
		}
		if err != nil || strings.TrimSpace(string(info)) != wantOS {
			t.Fatalf("explicit %s mode reached the wrong engine: %s, %v", contextName, info, err)
		}
		if !native {
			actual := mustCommand("docker", "inspect", "--format", "{{.Id}}", containerName)
			if strings.TrimSpace(string(actual)) != workload.ID {
				t.Fatalf("Linux mode selected a different workload: %s", actual)
			}
			if actual := mustCommand("docker", "exec", workload.ID, "cat", "/coexistence/dataset"); !bytes.Equal(actual, payload) {
				t.Fatal("mode selection lost real Linux volume contents")
			}
		} else if output, err := command(modeContext, "docker", "inspect", workload.ID); err == nil {
			t.Fatalf("native mode adopted a Linux workload from another endpoint: %s", output)
		}
	}
	selectMode(false, true)
	nativeAction(source, "install")
	first := recordInstallation(source, true)
	assertLinuxLive("native-install")
	nativeAction(source, "context")
	selectMode(true, true)
	assertLinuxLive("windows-mode-selected")
	selectMode(false, true)
	assertLinuxLive("linux-mode-reselected")

	updatedSource := filepath.Join(workspace, "native-coexistence-update", "porto.exe")
	buildDir := filepath.Join(filepath.Dir(updatedSource), "build")
	if err := os.MkdirAll(buildDir, 0o700); err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(mustCommand(source, "version"))) + "-coexistence-" + suite
	buildContext, stopBuild := context.WithTimeout(ctx, 2*time.Minute)
	buildCommand := process.NewCommand(buildContext, filepath.Dir(workspace), "go", "build", "-mod=readonly", "-buildvcs=false",
		"-ldflags=-X github.com/mbianchidev/porto/internal/config.Version="+version, "-o", updatedSource, "./cmd/porto")
	buildCommand.Env = process.WithEnvironment(environment, "CGO_ENABLED=0", "GOOS=windows", "GOARCH=amd64",
		"GOTMPDIR="+buildDir, "TMP="+buildDir, "TEMP="+buildDir)
	buildOutput, buildErr := buildCommand.CombinedOutput()
	stopBuild()
	if buildErr != nil {
		t.Fatalf("independently build changed current Porto CLI for a genuine binary update: %v: %s", buildErr, buildOutput)
	}
	if coexistenceFileHash(t, updatedSource) == first.Hashes["porto.exe"] ||
		strings.TrimSpace(string(mustCommand(updatedSource, "version"))) != version {
		t.Fatal("coexistence update binary is not an independently built, observably changed Porto CLI")
	}
	assertLinuxLive("changed-binary-built")
	nativeAction(updatedSource, "install")
	updated := recordInstallation(updatedSource, true)
	if updated.Generation == first.Generation || updated.Hashes["porto.exe"] == first.Hashes["porto.exe"] {
		t.Fatal("native binary update did not advance both protected generation and executable content")
	}
	assertLinuxLive("native-changed-binary-update")
	nativeAction(updatedSource, "install")
	repaired := recordInstallation(updatedSource, true)
	if repaired.Generation == updated.Generation || repaired.Hashes["porto.exe"] != updated.Hashes["porto.exe"] {
		t.Fatal("same-binary repair did not create a separate protected deployment with unchanged executable content")
	}
	assertLinuxLive("native-same-binary-repair")
	nativeAction(source, "stop")
	if stopped := recordInstallation(updatedSource, false); stopped.Generation != repaired.Generation {
		t.Fatal("native stop changed its deployment identity")
	}
	assertLinuxLive("native-stop")
	selectMode(true, false)
	assertLinuxLive("stopped-windows-mode-no-fallback")
	selectMode(false, true)
	nativeAction(source, "start")
	if started := recordInstallation(updatedSource, true); started.Generation != repaired.Generation {
		t.Fatal("native start changed its deployment identity")
	}
	assertLinuxLive("native-start")
	nativeAction(source, "remove")
	if status := windowscontainers.Snapshot(ctx); status.Installed || status.Running {
		t.Fatalf("native removal left a registered runtime: %+v", status)
	}
	if current, err := os.ReadFile(filepath.Join(expected.Root, "installation.json")); err != nil || !bytes.Equal(current, knownReceipt) {
		t.Fatalf("native service removal discarded or altered its recoverable store: %v", err)
	}
	assertLinuxLive("native-remove")
	selectMode(true, false)
	assertLinuxLive("removed-windows-mode-no-fallback")
	selectMode(false, true)
	nativeAction(source, "install")
	reinstalled := recordInstallation(source, true)
	if reinstalled.Generation == repaired.Generation {
		t.Fatal("native reinstall reused a removed service's protected deployment generation")
	}
	assertLinuxLive("native-reinstall")
	selectMode(true, true)
	assertLinuxLive("reinstalled-windows-mode-selected")
	selectMode(false, true)
	assertLinuxLive("final-linux-mode-selected")
	if err := cleanupNative(ctx); err != nil {
		t.Fatalf("clean up only the proven-owned native fixture while Linux remains live: %v", err)
	}
	assertLinuxLive("native-owned-cleanup")
	t.Log("real live Linux workload and volume survived native install, changed-binary update, same-binary repair, stop/start/remove/reinstall, owned cleanup and explicit Linux/Windows mode switches")
}
