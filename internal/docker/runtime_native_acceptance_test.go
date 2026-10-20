package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/nativefiles"
)

func (fixture *runtimeDataFixture) requireContainerFidelity(t *testing.T, selection dataops.Selection, source json.RawMessage) {
	t.Helper()
	document, err := fixture.manager.InspectContainer(fixture.ctx, selection.Destination)
	if err != nil {
		t.Fatal(err)
	}
	document, err = dockerInspectForVersion(document, "1.47", false)
	if err != nil {
		t.Fatal(err)
	}
	var before, after migrationContainer
	if err := json.Unmarshal(source, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document, &after); err != nil {
		t.Fatal(err)
	}
	client, closeRuntime, err := fixture.manager.storageRuntime(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()
	backend, ok := client.(*grpcContainerRuntime)
	if !ok || backend.client == nil {
		t.Fatal("real containerd configuration is unavailable")
	}
	ctx := namespaces.WithNamespace(fixture.ctx, backend.namespace)
	container, err := backend.client.LoadContainer(ctx, after.ID)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := container.Spec(ctx)
	if err != nil || spec.Process == nil {
		t.Fatalf("effective workload configuration unavailable: %v", err)
	}
	expectedArgs := append(slices.Clone(before.Config.Entrypoint), before.Config.Cmd...)
	actualNano := int64(0)
	if spec.Linux != nil && spec.Linux.Resources != nil && spec.Linux.Resources.CPU != nil {
		cpu := spec.Linux.Resources.CPU
		if cpu.Quota != nil && cpu.Period != nil && *cpu.Period > 0 {
			actualNano = *cpu.Quota * 1_000_000_000 / int64(*cpu.Period)
		}
	}
	for _, check := range []struct {
		name             string
		expected, actual any
	}{
		{"effective command", expectedArgs, spec.Process.Args},
		{"working directory", before.Config.WorkingDir, spec.Process.Cwd},
		{"hostname", before.Config.Hostname, after.Config.Hostname},
		{"healthcheck", before.Config.Healthcheck, after.Config.Healthcheck},
		{"restart policy", before.HostConfig.RestartPolicy, after.HostConfig.RestartPolicy},
		{"exposed ports", before.Config.ExposedPorts, after.Config.ExposedPorts},
		{"memory limit", before.HostConfig.Memory, after.HostConfig.Memory},
		{"swap limit", before.HostConfig.MemorySwap, after.HostConfig.MemorySwap},
		{"CPU limit", before.HostConfig.NanoCpus, actualNano},
	} {
		if !reflect.DeepEqual(check.expected, check.actual) {
			t.Errorf("stopped-container %s did not round trip", check.name)
		}
		for _, environment := range before.Config.Env {
			if !slices.Contains(spec.Process.Env, environment) {
				t.Error("a source environment value was not preserved in the workload specification")
			}
		}
	}
	for key, value := range before.Config.Labels {
		if after.Config.Labels[key] != value {
			t.Errorf("stopped-container label %s was not preserved", key)
		}
	}
	for _, mounted := range before.Mounts {
		destination := mounted.Name
		for _, selected := range fixture.selection {
			if selected.Kind == "volume" && selected.Name == mounted.Name {
				destination = selected.Destination
			}
		}
		if !slices.ContainsFunc(after.Mounts, func(current struct {
			Type, Name, Source, Destination string
			RW                              bool
		}) bool {
			return current.Type == "volume" && current.Name == destination && current.Destination == mounted.Destination && current.RW == mounted.RW
		}) {
			t.Errorf("stopped-container volume relationship %s was not preserved", mounted.Destination)
		}
	}
	for name, endpoint := range before.NetworkSettings.Networks {
		destination := name
		for _, selected := range fixture.selection {
			if selected.Kind == "network" && selected.Name == name {
				destination = selected.Destination
			}
		}
		var networks []string
		if err := json.Unmarshal([]byte(after.Config.Labels[nerdctlNetworksLabel]), &networks); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(networks, destination) {
			t.Errorf("stopped-container network %s was not preserved", name)
			continue
		}
		for _, alias := range endpoint.Aliases {
			if alias == before.ID || alias == before.ID[:min(12, len(before.ID))] {
				continue
			}
			if alias != after.Config.Hostname {
				t.Errorf("network alias %s was not preserved", alias)
			}
		}
	}
	if after.State.Running {
		t.Fatal("migration started a destination workload implicitly")
	}
}

func (fixture *runtimeDataFixture) backendWrite(t *testing.T, kind, name, relative string, contents []byte) {
	t.Helper()
	if kind == "vm" {
		output, err := fixture.vms.Exec(fixture.ctx, name, []string{"sh", "-c", `set -eu; mkdir -p -- "$(dirname -- "$1")"; cat > "$1"`, "porto-native-fixture", relative}, contents)
		if err != nil {
			t.Fatalf("write synthetic guest file: %v: %s", err, output)
		}
		return
	}
	descriptor, err := fixture.manager.FileDescriptor(fixture.ctx, kind, name)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := fixture.manager.FileRequest(fixture.ctx, kind, name, datafiles.Request{
		Action: "write", Path: relative, Identity: descriptor.Resource.Fingerprint(),
	}, bytes.NewReader(contents), &output); err != nil {
		t.Fatal(err)
	}
}

func (fixture *runtimeDataFixture) requireNativePaths(t *testing.T, kind, name string, attachment nativefiles.Attachment) {
	t.Helper()
	if kind != "image" {
		directory := strings.Repeat("long-", 30) + "\u03b4-" + newInventoryInstanceID()[:6]
		if err := os.Mkdir(filepath.Join(attachment.Path, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(attachment.Path, directory, "native-\u03b4.txt")
		content := []byte("synthetic long-path edit")
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if current, err := os.ReadFile(file); err != nil || !bytes.Equal(current, content) {
			t.Fatalf("long/non-ASCII native path failed: %q, %v", current, err)
		}
		guestFile := "guest-created-\u03b4-" + newInventoryInstanceID()[:6] + ".txt"
		fixture.backendWrite(t, kind, name, guestFile, []byte("concurrent guest change"))
		if current, err := os.ReadFile(filepath.Join(attachment.Path, guestFile)); err != nil || string(current) != "concurrent guest change" {
			t.Fatalf("guest changes were hidden from native access: %q, %v", current, err)
		}
	}
	if kind != "vm" {
		for _, test := range []struct{ name, value string }{{"Case.txt", "upper case fixture"}, {"case.txt", "lower case fixture"}, {"hardlink.txt", "synthetic runtime file\n"}} {
			content, err := os.ReadFile(filepath.Join(attachment.Path, test.name))
			if err != nil || string(content) != test.value {
				t.Fatalf("native case/hardlink semantics failed for %s: %q, %v", test.name, content, err)
			}
		}
		if _, err := os.ReadFile(filepath.Join(attachment.Path, "escape-link")); err == nil {
			t.Fatal("native access traversed an escaping source link")
		}
		content, err := os.ReadFile(filepath.Join(attachment.Path, "inside-link"))
		if runtime.GOOS == "darwin" || attachment.Local {
			if err == nil {
				t.Fatal("this backend must reject native symlink traversal")
			}
		} else if err != nil || string(content) != "synthetic runtime file\n" {
			t.Fatalf("contained native link did not resolve: %q, %v", content, err)
		}
	}
	if kind == "vm" {
		output, err := fixture.vms.Exec(fixture.ctx, name, []string{"sh", "-c", "printf 'synthetic denied fixture' > permission-denied.txt; chmod 000 permission-denied.txt"}, nil)
		if err != nil {
			t.Fatalf("prepare guest permission denial: %v: %s", err, output)
		}
		if _, err := os.ReadFile(filepath.Join(attachment.Path, "permission-denied.txt")); err == nil {
			t.Fatal("native VM access bypassed guest file permissions")
		}
	}
}

func (fixture *runtimeDataFixture) detachAfterDisconnect(t *testing.T, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fixture.native.Detach(ctx, id); err != nil {
		records := fixture.native.List()
		if !slices.ContainsFunc(records, func(record nativefiles.Attachment) bool { return record.ID == id && record.State == "detach-failed" }) {
			t.Fatalf("disconnect cleanup error lost its retryable record: %v", err)
		}
		if err := fixture.native.Detach(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func (fixture *runtimeDataFixture) nativeLifecycles(t *testing.T) {
	t.Run("local kernel mount identity and replaced-mount protection", fixture.localMountIdentity)
	t.Run("explicit workload start and stop", func(t *testing.T) {
		selected := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "container" })]
		if err := fixture.manager.ContainerAction(fixture.ctx, selected.Destination, "start"); err != nil {
			t.Fatal(err)
		}
		document, err := fixture.manager.InspectContainer(fixture.ctx, selected.Destination)
		if err != nil {
			t.Fatal(err)
		}
		var inspected migrationContainer
		if err := json.Unmarshal(document, &inspected); err != nil {
			t.Fatal(err)
		}
		bindings := inspected.HostConfig.PortBindings["8080/tcp"]
		if len(bindings) != 1 || bindings[0].HostPort == "" {
			t.Fatal("migrated published port was not activated")
		}
		url := "http://127.0.0.1:" + bindings[0].HostPort + "/index.html"
		httpClient := &http.Client{Timeout: time.Second}
		deadline := time.Now().Add(time.Minute)
		for {
			response, err := httpClient.Get(url)
			if err == nil {
				var content bytes.Buffer
				_, copyErr := content.ReadFrom(response.Body)
				closeErr := response.Body.Close()
				if copyErr == nil && closeErr == nil && response.StatusCode == http.StatusOK && content.String() == "porto-runtime-native-fixture\n" {
					break
				}
			}
			if time.Now().After(deadline) {
				probe, probeErr := fixture.manager.ExecContainer(fixture.ctx, selected.Destination, []string{"wget", "-qO-", "http://127.0.0.1:8080/index.html"}, nil)
				t.Logf("synthetic in-container service probe: %q, %v", probe, probeErr)
				t.Fatalf("the migrated workload was not reachable through its preserved port: %v", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
		target, err := fixture.nativeTarget("container", selected.Destination, true)
		if err != nil {
			t.Fatal(err)
		}
		record, err := fixture.native.Attach(fixture.ctx, target, true)
		if err != nil {
			t.Fatal(err)
		}
		fixture.requireNativePaths(t, "container", selected.Destination, record)
		if err := os.WriteFile(filepath.Join(record.Path, "readonly", "forbidden-running.txt"), []byte("forbidden"), 0o600); err == nil {
			t.Fatal("running native access bypassed a read-only mount")
		}
		if err := fixture.manager.ContainerAction(fixture.ctx, selected.Destination, "stop"); !errors.Is(err, datafiles.ErrConflict) {
			t.Fatalf("Porto stopped an attached workload instead of requiring detach: %v", err)
		}
		if err := fixture.native.Detach(fixture.ctx, record.ID); err != nil {
			t.Fatal(err)
		}
		if err := fixture.manager.ContainerAction(fixture.ctx, selected.Destination, "stop"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("deleted and recreated volume identities", func(t *testing.T) {
		name := fixture.prefix + "-identity-volume"
		selection := dataops.Selection{Kind: "volume", Name: name, Destination: name}
		fixture.extraSelections = append(fixture.extraSelections, selection)
		if _, err := fixture.manager.CreateVolume(fixture.ctx, name, "local", map[string]string{"porto.test.suite": fixture.prefix}); err != nil {
			t.Fatal(err)
		}
		target, err := fixture.nativeTarget("volume", name, true)
		if err != nil {
			t.Fatal(err)
		}
		record, err := fixture.native.Attach(fixture.ctx, target, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.manager.RemoveVolume(fixture.ctx, name, false); !errors.Is(err, datafiles.ErrConflict) {
			t.Fatalf("attached volume removal bypassed lifecycle protection: %v", err)
		}
		if _, err := fixture.manager.run(fixture.ctx, "delete exact synthetic volume outside Porto to exercise identity loss", "volume", "rm", name); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.manager.CreateVolume(fixture.ctx, name, "local", map[string]string{"porto.test.suite": fixture.prefix}); err != nil {
			t.Fatal(err)
		}
		current, _, err := fixture.manager.NativeFilesResource(fixture.ctx, "volume", name, true)
		if err != nil || current.Fingerprint() == record.Identity {
			t.Fatalf("recreated native volume retained its old identity: %+v, %v", current, err)
		}
		disconnected, err := fixture.native.Get(fixture.ctx, record.ID)
		if err == nil || disconnected.State != "disconnected" {
			t.Fatalf("old native mount followed a recreated resource: %+v, %v", disconnected, err)
		}
		fixture.detachAfterDisconnect(t, record.ID)
		if current, _, err := fixture.manager.NativeFilesResource(fixture.ctx, "volume", name, true); err != nil || current.Fingerprint() == record.Identity {
			t.Fatalf("owned cleanup removed or retargeted replacement data: %+v, %v", current, err)
		}
	})
	t.Run("standalone VM loss and explicit reconnect", func(t *testing.T) {
		target, err := fixture.nativeTarget("vm", fixture.instance.Name, true)
		if err != nil {
			t.Fatal(err)
		}
		record, err := fixture.native.Attach(fixture.ctx, target, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.vms.Stop(fixture.ctx, fixture.instance.Name); err != nil {
			t.Fatal(err)
		}
		current, err := fixture.native.Get(fixture.ctx, record.ID)
		if err == nil || current.State != "disconnected" {
			t.Fatalf("stopped VM remained advertised connected: %+v, %v", current, err)
		}
		fixture.detachAfterDisconnect(t, record.ID)
		if _, err := fixture.vms.NativeFilesResource(fixture.ctx, fixture.instance.Name); !errors.Is(err, datafiles.ErrUnsupported) {
			t.Fatalf("stopped VM became implicitly accessible: %v", err)
		}
		if err := fixture.vms.Start(fixture.ctx, fixture.instance.Name); err != nil {
			t.Fatal(err)
		}
		target, err = fixture.nativeTarget("vm", fixture.instance.Name, true)
		if err != nil {
			t.Fatal(err)
		}
		reconnected, err := fixture.native.Attach(fixture.ctx, target, true)
		if err != nil {
			t.Fatal(err)
		}
		if reconnected.Identity != record.Identity || !filepath.IsAbs(reconnected.Path) {
			t.Fatal("reconnect changed the original VM identity")
		}
		if err := fixture.native.Close(fixture.ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func (fixture *runtimeDataFixture) backendPathCommand(t *testing.T, arguments ...string) []byte {
	t.Helper()
	backend, err := fixture.manager.backend(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var output []byte
	if backend.limaInstance != "" {
		output, err = fixture.manager.runCommand(fixture.ctx, time.Minute, "prepare owned data acceptance filesystem", nil, backend.name, append([]string{"shell", "--workdir=/", backend.limaInstance, "--"}, arguments...)...)
	} else {
		output, err = fixture.manager.runCommand(fixture.ctx, time.Minute, "prepare owned data acceptance filesystem", nil, arguments[0], arguments[1:]...)
	}
	if err != nil {
		t.Fatalf("prepare isolated test filesystem: %v: %.4096s", err, output)
	}
	return output
}

func (fixture *runtimeDataFixture) runtimeLowSpaceRoot(t *testing.T) string {
	t.Helper()
	root := path.Join("/tmp", fixture.prefix+"-low-space")
	uid := strings.TrimSpace(string(fixture.backendPathCommand(t, "id", "-u")))
	gid := strings.TrimSpace(string(fixture.backendPathCommand(t, "id", "-g")))
	fixture.backendPathCommand(t, "sudo", "-n", "mkdir", "--", root)
	fixture.backendPathCommand(t, "sudo", "-n", "mount", "-t", "tmpfs", "-o", "size=8m,mode=0700,uid="+uid+",gid="+gid, "tmpfs", root)
	t.Cleanup(func() {
		fixture.backendPathCommand(t, "sudo", "-n", "umount", "--", root)
		fixture.backendPathCommand(t, "sudo", "-n", "rmdir", "--", root)
	})
	capacity := strings.Fields(string(fixture.backendPathCommand(t, "stat", "-f", "-c", "%b %S %T", "--", root)))
	if len(capacity) != 3 || capacity[2] != "tmpfs" {
		t.Fatalf("low-space fixture is not an actual tmpfs: %v", capacity)
	}
	blocks, blocksErr := strconv.ParseUint(capacity[0], 10, 64)
	blockSize, sizeErr := strconv.ParseUint(capacity[1], 10, 64)
	if blocksErr != nil || sizeErr != nil || blockSize == 0 || 8*1024*1024%blockSize != 0 || blocks != 8*1024*1024/blockSize {
		t.Fatalf("low-space fixture is not exactly 8 MiB: %v, %v, %v", capacity, blocksErr, sizeErr)
	}
	return root
}
