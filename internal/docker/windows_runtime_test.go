package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func nativeTestInstallation(t *testing.T) windowscontainers.Installation {
	t.Helper()
	return windowscontainers.Installation{Version: 1, OwnerSID: "S-1-5-21-111-222-333-1001",
		Generation: strings.Repeat("a", 32), Root: t.TempDir(),
		Host: windowscontainers.Host{OS: "windows", Architecture: "amd64", Build: 26100,
			Edition: "ServerDatacenter", Server: true, Containers: true}}
}

func nativeTestManager(t *testing.T, runner runtimes.Runner) *Manager {
	t.Helper()
	manager, err := newWindowsManager(runner, nativeTestInstallation(t))
	if err != nil {
		t.Fatal(err)
	}
	manager.windows.checkNetworks = func(windowscontainers.Installation) error { return nil }
	manager.windows.checkBind = func(string) error { return nil }
	return manager
}

func nativeCommandArguments(t *testing.T, manager *Manager, command runtimes.Command) []string {
	t.Helper()
	prefix := manager.windows.installation.NerdctlArgs()
	if command.Name != filepath.Join(manager.windows.installation.Bin(), "nerdctl.exe") ||
		len(command.Args) < len(prefix) || !reflect.DeepEqual(command.Args[:len(prefix)], prefix) {
		t.Fatalf("command escaped owned native runtime: %+v", command)
	}
	return command.Args[len(prefix):]
}

func TestNativeWindowsNeverDiscoversAnExternalRuntimeOrLinuxHelpers(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	manager.lookPath = func(name string) (string, error) {
		t.Fatalf("searched PATH for %s", name)
		return "", errors.New("not found")
	}
	t.Setenv("CONTAINERD_ADDRESS", `\\.\pipe\foreign-containerd`)
	t.Setenv("NERDCTL_NAMESPACE", "foreign")
	backend, err := manager.backend(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if backend.name != filepath.Join(manager.windows.installation.Bin(), "nerdctl.exe") ||
		strings.Contains(strings.Join(backend.prefix, " "), "foreign") {
		t.Fatalf("backend = %+v", backend)
	}
	for _, argument := range backend.prefix {
		if argument == "--config" || argument == "--runtime" {
			t.Fatalf("unsupported nerdctl global flag: %s", argument)
		}
	}
	if manager.creationConnector != nil || manager.operationsConnector != nil || manager.execConnector != nil ||
		manager.networkConnector != nil || manager.metricReader != nil {
		t.Fatal("native manager retained a Linux operation connector")
	}
	for _, check := range []func() error{
		func() error { _, err := manager.runtimeHelperPath(); return err },
		func() error { _, err := manager.Build(context.Background(), BuildRequest{}); return err },
		func() error { _, err := manager.Builds(context.Background()); return err },
		func() error { return manager.StartEngine(context.Background()) },
		func() error { return manager.StopEngine(context.Background()) },
		func() error { return manager.RemoveEngine(context.Background()) },
	} {
		if err := check(); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("native unsupported operation = %v", err)
		}
	}
	if len(runner.commands) != 0 {
		t.Fatal("unsupported native operation executed a helper")
	}
	adapter := windowsInventory{}
	if _, ok := any(adapter).(containerCreation); ok {
		t.Fatal("native inventory exposes direct Linux creation")
	}
	if _, ok := any(adapter).(containerOperations); ok {
		t.Fatal("native inventory exposes direct Linux lifecycle")
	}
	if _, ok := any(adapter).(execOperations); ok {
		t.Fatal("native inventory exposes direct Linux FIFO exec")
	}
	if _, ok := any(adapter).(networkOperations); ok {
		t.Fatal("native inventory exposes Linux helper networking")
	}
}

func TestNativeWindowsCreationValidatesBeforeMutationAndPreservesBindArguments(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	var created runtimes.Command
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		switch args[0] {
		case "image":
			return []byte(`[{"Id":"sha256:` + strings.Repeat("3", 64) + `","Descriptor":{"digest":"sha256:` +
				strings.Repeat("1", 64) + `"},"Os":"windows","Architecture":"amd64","OsVersion":"10.0.26100.1000"}]`), nil
		case "create":
			created = command
			return []byte("owned-container-id\n"), nil
		default:
			t.Fatalf("unexpected native command: %+v", command)
			return nil, nil
		}
	}
	mount, err := dockerCSV([]string{"type=bind", `source=C:\porto-é's fixture,one`, `target=C:\data`})
	if err != nil {
		t.Fatal(err)
	}
	request := CreateContainerRequest{Name: "native-fixture", Image: "registry.example.test/native:v1",
		Isolation: "process", Mounts: []string{mount}, Command: []string{"cmd.exe", "/S", "/C", "echo synthetic"}}
	id, err := manager.CreateContainer(context.Background(), request)
	if err != nil || id != "owned-container-id" {
		t.Fatalf("create = %q, %v", id, err)
	}
	args := nativeCommandArguments(t, manager, created)
	for _, value := range []string{"--runtime", windowscontainers.RuntimeName, "--isolation", "process", "--pull=never", mount, manager.windows.installation.DefaultNetwork(),
		"sha256:" + strings.Repeat("1", 64), "echo synthetic"} {
		found := false
		for _, argument := range args {
			if argument == value {
				found = true
			}
		}
		if !found {
			t.Fatalf("native create lost %q: %q", value, args)
		}
	}
	for _, bad := range []struct {
		name string
		edit func(*CreateContainerRequest)
	}{
		{"hostprocess", func(r *CreateContainerRequest) { r.Isolation = "host" }},
		{"linux-platform", func(r *CreateContainerRequest) { r.Platform = "linux/amd64" }},
		{"arm64", func(r *CreateContainerRequest) { r.Platform = "windows/arm64" }},
		{"foreign-network", func(r *CreateContainerRequest) { r.Networks = []ContainerNetwork{{Name: "porto-another-owner-nat"}} }},
		{"Linux-bind", func(r *CreateContainerRequest) { r.Mounts = nil; r.Volumes = []string{"/mnt/c/source:/data"} }},
		{"UNC-bind", func(r *CreateContainerRequest) {
			r.Mounts = nil
			r.Volumes = []string{`\\wsl.localhost\Other\root:C:\data`}
		}},
		{"privileged", func(r *CreateContainerRequest) { r.Privileged = true }},
		{"linux-limits", func(r *CreateContainerRequest) { r.Limits.CPUQuota = 100000 }},
	} {
		t.Run(bad.name, func(t *testing.T) {
			created = runtimes.Command{}
			candidate := request
			bad.edit(&candidate)
			if _, err := manager.CreateContainer(context.Background(), candidate); err == nil {
				t.Fatal("unsupported native request succeeded")
			}
			if created.Name != "" {
				t.Fatal("unsupported native request mutated container state")
			}
		})
	}
}

func TestNativeWindowsImageBuildMismatchDoesNotCreateOrPull(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if args[0] != "image" {
			t.Fatalf("incompatible image mutated native state: %q", args)
		}
		return []byte(`[{"Id":"synthetic","Descriptor":{"digest":"sha256:` + strings.Repeat("1", 64) +
			`"},"Os":"windows","Architecture":"amd64","OsVersion":"10.0.20348.1"}]`), nil
	}
	if _, err := manager.CreateContainer(context.Background(), CreateContainerRequest{Image: "synthetic", Isolation: "process"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("process build mismatch = %v", err)
	}
	for _, platform := range []windowscontainers.ImagePlatform{
		{OS: "linux", Architecture: "amd64"}, {OS: "windows", Architecture: "arm64", OSVersion: "10.0.26100.1"},
		{OS: "windows", Architecture: "amd64", OSVersion: "10.0.20348.1"},
	} {
		manager.windows.resolveImage = func(context.Context, string, *RegistryAuth) (windowscontainers.ImagePlatform, string, error) {
			return platform, "registry.example.test/synthetic@sha256:" + strings.Repeat("1", 64), nil
		}
		before := len(runner.commands)
		if err := manager.PullImage(context.Background(), "synthetic", ""); err == nil {
			t.Fatal("incompatible image was pulled")
		}
		if len(runner.commands) != before {
			t.Fatal("image preflight mutated the store")
		}
	}
}

func TestNativeWindowsDockerMetadataAndUnsupportedSurfaces(t *testing.T) {
	manager := nativeTestManager(t, &fakeRunner{})
	api := NewAPI(manager, manager.windows.installation.Pipe())
	for _, path := range []string{"/version", "/v1.47/version", "/_ping"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Header().Get("Ostype") != "windows" {
			t.Fatalf("native OS header = %v", response.Header())
		}
		if strings.HasSuffix(path, "version") {
			var value struct {
				OS string `json:"Os"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.OS != "windows" {
				t.Fatalf("native version = %s", response.Body.String())
			}
		}
	}
	for _, path := range []string{"/build", "/grpc", "/session", "/containers/a/archive", "/containers/a/checkpoints", "/images/load", "/system/df"} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1.47"+path, nil))
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("native surface %s = %d: %s", path, response.Code, response.Body.String())
		}
	}
}

func TestNativeWindowsArchitectureGateNeverUsesAnX64Substitute(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		if _, err := NewWindowsManager(&fakeRunner{}, nativeTestInstallation(t)); !errors.Is(err, windowscontainers.ErrUnsupported) {
			t.Fatalf("native platform gate = %v", err)
		}
	}
	installation := nativeTestInstallation(t)
	installation.Host.Architecture = "arm64"
	if _, err := newWindowsManager(&fakeRunner{}, installation); err == nil {
		t.Fatal("ARM64 used AMD64 native tools")
	}
}

func TestNativeWindowsPinnedIndexResolutionSurvivesWorkerRestart(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	original := "registry.example.test/synthetic@sha256:" + strings.Repeat("1", 64)
	selected := "registry.example.test/synthetic@sha256:" + strings.Repeat("2", 64)
	manager.windows.resolveImage = func(context.Context, string, *RegistryAuth) (windowscontainers.ImagePlatform, string, error) {
		return windowscontainers.ImagePlatform{OS: "windows", Architecture: "amd64", OSVersion: "10.0.26100.1"}, selected, nil
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if args[0] != "pull" || args[len(args)-1] != selected {
			t.Fatalf("pull did not pin the validated manifest: %q", args)
		}
		return nil, nil
	}
	if err := manager.PullImage(context.Background(), original, ""); err != nil {
		t.Fatal(err)
	}
	restarted, err := newWindowsManager(runner, manager.windows.installation)
	if err != nil {
		t.Fatal(err)
	}
	restarted.windows.checkNetworks = func(windowscontainers.Installation) error { return nil }
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, restarted, command)
		if args[0] != "image" || args[len(args)-1] != "sha256:"+strings.Repeat("2", 64) {
			t.Fatalf("original index reference did not resolve after restart: %q", args)
		}
		return []byte(`[{"Id":"sha256:` + strings.Repeat("3", 64) + `","Descriptor":{"digest":"sha256:` +
			strings.Repeat("2", 64) + `"},"Os":"windows","Architecture":"amd64","OsVersion":"10.0.26100.1"}]`), nil
	}
	if _, err := restarted.InspectImage(context.Background(), original, "windows/amd64"); err != nil {
		t.Fatal(err)
	}
	if _, target, err := restarted.localWindowsImage(context.Background(), original); err != nil ||
		target != "sha256:"+strings.Repeat("2", 64) {
		t.Fatalf("native creation confused config ID with immutable manifest target: %s, %v", target, err)
	}
	data, _ := json.Marshal(map[string]string{original: "another.example.test/foreign@sha256:" + strings.Repeat("2", 64)})
	if err := os.WriteFile(filepath.Join(manager.stateDir, "native-image-references.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newWindowsManager(runner, manager.windows.installation); !errors.Is(err, windowscontainers.ErrOwnership) {
		t.Fatalf("cross-repository native reference was adopted: %v", err)
	}
}

func TestNativeWindowsDigestInspectionUsesVerifiedOCIWindowsVersion(t *testing.T) {
	for _, failure := range []string{"", "missing-version", "changed-config", "wrong-platform"} {
		t.Run(failure, func(t *testing.T) {
			runner := &fakeRunner{}
			manager := nativeTestManager(t, runner)
			target := "sha256:" + strings.Repeat("2", 64)
			config := "sha256:" + strings.Repeat("3", 64)
			version, architecture := "10.0.26100.1234", "amd64"
			if failure == "missing-version" {
				version = ""
			} else if failure == "changed-config" {
				config = "sha256:" + strings.Repeat("4", 64)
			} else if failure == "wrong-platform" {
				architecture = "arm64"
			}
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				args := nativeCommandArguments(t, manager, command)
				if args[0] != "image" || args[1] != "inspect" || args[len(args)-1] != target {
					t.Fatalf("native digest inspection changed immutable identity: %q", args)
				}
				if strings.Contains(strings.Join(args, " "), "--mode native") {
					return json.Marshal([]any{map[string]any{
						"Image":           map[string]any{"target": map[string]string{"digest": target}},
						"ImageConfigDesc": map[string]string{"digest": config},
						"ImageConfig":     map[string]string{"os": "windows", "architecture": architecture, "os.version": version}}})
				}
				return []byte(`[{"Id":"sha256:` + strings.Repeat("3", 64) +
					`","Os":"windows","Architecture":"amd64","Config":{"Cmd":["cmd.exe"]}}]`), nil
			}
			document, err := manager.InspectImage(context.Background(), "registry.example.test/synthetic@"+target, "windows/amd64")
			if failure != "" {
				if err == nil {
					t.Fatalf("unverified Windows image metadata was accepted: %s", document)
				}
				return
			}
			var inspected struct {
				OSVersion  string `json:"OsVersion"`
				Config     struct{ Cmd []string }
				Descriptor struct{ Digest string }
			}
			if err != nil || json.Unmarshal(document, &inspected) != nil || inspected.OSVersion != version ||
				inspected.Descriptor.Digest != target ||
				!reflect.DeepEqual(inspected.Config.Cmd, []string{"cmd.exe"}) {
				t.Fatalf("native OCI metadata integration: %s, %v", document, err)
			}
		})
	}
}

func TestNativeWindowsImageConfigIDsResolveToImmutableManifestTargets(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	config, target := "sha256:"+strings.Repeat("3", 64), "sha256:"+strings.Repeat("2", 64)
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if args[0] == "images" {
			return json.Marshal(map[string]string{"ID": config, "Digest": target, "Repository": "registry.example.test/synthetic", "Tag": "<none>"})
		}
		if args[0] == "tag" {
			if args[1] != "registry.example.test/synthetic@"+target {
				t.Fatalf("native image tag lost its stored source identity: %q", args)
			}
			return nil, nil
		}
		if args[0] == "image" && args[1] == "rm" && args[len(args)-1] == target {
			return nil, nil
		}
		if args[0] != "image" || args[1] != "inspect" || args[len(args)-1] != target {
			t.Fatalf("native image configuration ID was sent to the manifest-only runtime: %q", args)
		}
		return []byte(`[{"Id":"` + config + `","Descriptor":{"digest":"` + target +
			`"},"Config":{"Image":"registry.example.test/synthetic@` + target +
			`"},"Os":"windows","Architecture":"amd64","OsVersion":"10.0.26100.1"}]`), nil
	}
	for _, id := range []string{config, strings.Repeat("3", 12), target} {
		if _, selected, err := manager.localWindowsImage(context.Background(), id); err != nil || selected != target {
			t.Fatalf("native image ID resolution %s: selected=%s error=%v", id, selected, err)
		}
		if err := manager.TagImage(context.Background(), id, "registry.example.test/tagged:v1"); err != nil {
			t.Fatalf("native image ID tagging %s: %v", id, err)
		}
		if err := manager.RemoveImage(context.Background(), id, true); err != nil {
			t.Fatalf("native image ID removal %s: %v", id, err)
		}
	}
}

func TestNativeWindowsMissingImageIDCannotSucceedThroughForcedRemoval(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if args := nativeCommandArguments(t, manager, command); args[0] != "images" {
			t.Fatalf("missing native image ID reached a mutation: %q", args)
		}
		return nil, nil
	}
	if err := manager.RemoveImage(context.Background(), "sha256:"+strings.Repeat("9", 64), true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing native image ID removal: %v", err)
	}
}

func TestNativeWindowsLogicalNetworksNeverAdoptAnUnownedHNSName(t *testing.T) {
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	ownedName, err := manager.ownedWindowsNetworkName("developer-network")
	if err != nil {
		t.Fatal(err)
	}
	cniCreated := false
	manager.windows.ensureNetwork = func(i windowscontainers.Installation, name, subnet string) (windowscontainers.Network, error) {
		if err := windowscontainers.ValidateNetworkName(i, name); err != nil {
			t.Fatal(err)
		}
		if name == "developer-network" || subnet == "" {
			t.Fatalf("native network escaped private naming/subnet: %s, %s", name, subnet)
		}
		if !cniCreated {
			t.Fatal("HNS was allocated before nerdctl could validate and create its CNI subnet")
		}
		return windowscontainers.Network{Name: name, ID: "owned-hns-id", Subnet: subnet}, nil
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if args[0] != "network" || args[1] != "create" || args[len(args)-1] != ownedName {
			t.Fatalf("unexpected native network mutation %q", args)
		}
		cniCreated = true
		return []byte("owned-network-id\n"), nil
	}
	if _, err := manager.CreateNetwork(context.Background(), CreateNetworkRequest{Name: "developer-network"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := manager.windowsNetworkIdentifier(context.Background(), "developer-network")
	if err != nil || resolved != ownedName {
		t.Fatalf("native logical name did not resolve privately: %s, %v", resolved, err)
	}
	for _, bad := range []string{"host", "bridge", "porto-another-owner-nat", "../foreign"} {
		if _, err := manager.ownedWindowsNetworkName(bad); err == nil {
			t.Fatalf("unowned native network accepted: %s", bad)
		}
	}
}

func TestNativeWindowsNetworkHNSFailureRollsBackOnlyNewCNIConfiguration(t *testing.T) {
	for _, rollbackFails := range []bool{false, true} {
		t.Run(fmt.Sprint(rollbackFails), func(t *testing.T) {
			runner := &fakeRunner{}
			manager := nativeTestManager(t, runner)
			hnsError := errors.New("synthetic HNS failure")
			rollbackError := errors.New("synthetic CNI rollback failure")
			manager.windows.ensureNetwork = func(windowscontainers.Installation, string, string) (windowscontainers.Network, error) {
				return windowscontainers.Network{}, hnsError
			}
			var mutations []string
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				args := nativeCommandArguments(t, manager, command)
				if len(args) < 3 || args[0] != "network" {
					t.Fatalf("unexpected native network command: %q", args)
				}
				mutations = append(mutations, args[1])
				if args[1] == "create" {
					return []byte("new-cni-network-id\n"), nil
				}
				if args[1] != "rm" || args[2] != "new-cni-network-id" {
					t.Fatalf("rollback escaped the newly created CNI identity: %q", args)
				}
				if rollbackFails {
					return nil, rollbackError
				}
				return nil, nil
			}
			id, err := manager.CreateNetwork(context.Background(), CreateNetworkRequest{Name: "synthetic-network"})
			if id != "" || !errors.Is(err, hnsError) || (rollbackFails && !errors.Is(err, rollbackError)) ||
				!reflect.DeepEqual(mutations, []string{"create", "rm"}) {
				t.Fatalf("native network rollback: id=%q mutations=%q error=%v", id, mutations, err)
			}
		})
	}
}

func TestNativeWindowsDriveVolumeParsingPreservesQuotesAndUnicode(t *testing.T) {
	source, target, mode, err := windowsVolumeParts(`C:\quote's é path:C:\data:ro`)
	if err != nil || source != `C:\quote's é path` || target != `C:\data` || mode != "ro" {
		t.Fatalf("drive volume = %q %q %q %v", source, target, mode, err)
	}
	for _, invalid := range []string{"/root:/data", `C:relative:C:\data`, `\\host\share:C:\data`, `C:\source:/data`} {
		if _, _, _, err := windowsVolumeParts(invalid); err == nil {
			t.Fatalf("accepted invalid native volume %q", invalid)
		}
		for _, path := range []string{`C:\`, `C:\ProgramData`, `C:\ProgramData\PortoWindowsContainers-owned\versions\a`} {
			if !windowsPathsOverlap(path, `C:\ProgramData\PortoWindowsContainers-owned`) {
				t.Fatalf("protected native runtime could be exposed through %s", path)
			}
		}
		if windowsPathsOverlap(`C:\Users\developer\workspace`, `C:\ProgramData\PortoWindowsContainers-owned`) {
			t.Fatal("unrelated developer bind was rejected")
		}
	}
}
