package extensions

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func installApproved(t *testing.T, m *Manager, p Package, grants Grants) Installed {
	t.Helper()
	p.SHA256 = Digest(p.Content)
	value, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(value.ID, Configuration{Enabled: true, Approve: true, Grants: grants}); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestBrokerDeniedSurfacesNamedSecretsAndHostFiles(t *testing.T) {
	m := testManager(t, func(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("forbidden request reached core API")
		return nil, nil
	})
	p := fixturePackage(t, "1.0.0", `{"capability":"runtime.read","method":"GET","path":"/api/registries"}`)
	value := installApproved(t, m, p, Grants{Permissions: []string{RuntimeRead}, Environments: []string{"local"}})
	if _, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("extension reached credential management")
	}
	directory := t.TempDir()
	selected, unselected := filepath.Join(directory, "selected"), filepath.Join(directory, "unselected")
	for _, path := range []string{selected, unselected} {
		if err := os.WriteFile(path, []byte("synthetic-content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p = fixturePackage(t, "2.0.0", `{"capability":"host.files.read","file":`+strconv.Quote(unselected)+`}`)
	p.Content.Manifest.Permissions = []string{FilesRead}
	value = installApproved(t, m, p, Grants{Permissions: []string{FilesRead}, Files: []string{selected}})
	if _, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("unselected host file was read")
	}
	p = fixturePackage(t, "3.0.0", `{"capability":"secrets.read","secret":"synthetic-secret"}`)
	p.Content.Manifest.Permissions = []string{SecretsRead}
	value = installApproved(t, m, p, Grants{Permissions: []string{SecretsRead}, Secrets: []string{"synthetic-secret"}})
	if _, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("unstored or another extension's secret was returned")
	}
	if err := m.SetSecret(value.ID, "synthetic-secret", "synthetic-private-value"); err != nil {
		t.Fatal(err)
	}
	result, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"})
	if err != nil || !strings.Contains(string(result.Data), "synthetic-private-value") {
		t.Fatalf("explicit named secret interface: %s %v", result.Data, err)
	}
	if err := m.Remove(value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.vault.Get("dev.mbianchi.porto.extensions", m.secretAccount(value.ID, "synthetic-secret")); err == nil {
		t.Fatal("uninstall retained extension-owned secret")
	}
}

func TestRevokedGrantsCancelActiveBrokerHandles(t *testing.T) {
	started := make(chan struct{})
	m := testManager(t, func(ctx context.Context, _, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	value := installApproved(t, m, fixturePackage(t, "1.0.0", `{"capability":"runtime.read","method":"GET","path":"/api/docker/containers"}`), Grants{Permissions: []string{RuntimeRead}, Environments: []string{"local"}})
	done := make(chan error, 1)
	go func() {
		_, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("broker did not start")
	}
	if err := m.Configure(value.ID, Configuration{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked action reported success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked broker handle survived")
	}
	m.mu.Lock()
	active := len(m.active)
	m.mu.Unlock()
	if active != 0 {
		t.Fatal("canceled extension retained handles")
	}
}

func TestInterruptedUpdateRetainsPreviousArtifactAndGrants(t *testing.T) {
	m := testManager(t, nil)
	value := installApproved(t, m, fixturePackage(t, "1.0.0", `{"capability":"result","data":{"version":1}}`), Grants{})
	state := filepath.Join(m.root, "state.json")
	backup := filepath.Join(m.root, "synthetic-state-backup")
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	p := fixturePackage(t, "2.0.0", `{"capability":"result","data":{"version":2}}`)
	if _, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true}); err == nil {
		t.Fatal("interrupted update claimed success")
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, state); err != nil {
		t.Fatal(err)
	}
	current := m.List()[0]
	if current.Version != value.Version || !current.Enabled || current.Generation != value.Generation+1 {
		t.Fatalf("interrupted update changed committed installation: %+v", current)
	}
	result, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"})
	if err != nil || !strings.Contains(string(result.Data), `"version":1`) {
		t.Fatalf("rollback failed: %s %v", result.Data, err)
	}
}

func TestHostImportsAndPrivateNetworkDestinationsFailClosed(t *testing.T) {
	m := testManager(t, nil)
	p := fixturePackage(t, "1.0.0", `{"capability":"result","data":{}}`)
	p.Content.Module = bytes.ReplaceAll(p.Content.Module, []byte("wasi_snapshot_preview1"), []byte("wasi_snapshot_preview2"))
	p.SHA256 = Digest(p.Content)
	if _, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true}); err == nil {
		t.Fatal("arbitrary host imports accepted")
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "100.100.100.200"} {
		if publicIP(net.ParseIP(address)) {
			t.Fatalf("private/metadata address permitted: %s", address)
		}
	}
	p = fixturePackage(t, "1.0.0", `{"capability":"network.fetch","url":"https://127.0.0.1/synthetic"}`)
	p.Content.Manifest.Permissions = []string{NetworkFetch}
	value := installApproved(t, m, p, Grants{Permissions: []string{NetworkFetch}, Destinations: []string{"https://127.0.0.1"}})
	if _, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("private HTTPS egress bypassed broker")
	}
}

func TestSeparatelyPackagedReferenceIntegrationRunsWithoutHostPrivileges(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", filepath.Join(directory, "action.wasm"), "../../examples/extensions/resource-summary/main.go")
	command.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build portable reference module: %v\n%s", err, output)
	}
	for _, name := range []string{"manifest.json", "view.html"} {
		data, err := os.ReadFile(filepath.Join("../../examples/extensions/resource-summary", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Pack(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := testManager(t, func(_ context.Context, environment, method, path string, _ json.RawMessage) (json.RawMessage, error) {
		if environment != "local" || method != "GET" || path != "/api/docker/containers" {
			t.Fatalf("reference escaped grant: %s %s %s", environment, method, path)
		}
		return json.RawMessage(`[{"name":"synthetic-reference-container"}]`), nil
	})
	value := installApproved(t, m, p, Grants{Permissions: []string{RuntimeRead}, Environments: []string{"local"}})
	result, err := m.Invoke(context.Background(), value.ID, Invocation{Action: "inventory", EnvironmentID: "local"})
	if err != nil || !strings.Contains(string(result.Data), "synthetic-reference-container") {
		t.Fatalf("reference integration: %s %v", result.Data, err)
	}
	if _, err := m.UI(value.ID, value.Generation); err == nil {
		t.Fatal("stale pre-approval UI generation remained accessible")
	}
	if runtime.GOOS == "windows" {
		if err := validateGrants(Manifest{Permissions: []string{ProcessExec}}, Grants{Permissions: []string{ProcessExec}}); err == nil {
			t.Fatal("unsupported Windows native process isolation was silently accepted")
		}
	}
}
