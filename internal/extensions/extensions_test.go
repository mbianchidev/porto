package extensions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"github.com/zalando/go-keyring"
	"strings"
	"sync"
	"testing"
	"time"
)

type testVault struct {
	mu   sync.Mutex
	data map[string]string
}

func (v *testVault) Set(service, account, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.data == nil {
		v.data = map[string]string{}
	}
	v.data[service+"/"+account] = value
	return nil
}
func (v *testVault) Get(service, account string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.data[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (v *testVault) Delete(service, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.data, service+"/"+account)
	return nil
}

func leb(value int) []byte {
	out := []byte{}
	for {
		b := byte(value & 127)
		value >>= 7
		if value != 0 {
			b |= 128
		}
		out = append(out, b)
		if value == 0 {
			return out
		}
	}
}
func wasmString(value string) []byte { return append(leb(len(value)), []byte(value)...) }

// A synthetic WASI program writes one broker request, without a compiler or real data.
func fixtureModule(output string, loop bool) []byte {
	module := []byte{0, 97, 115, 109, 1, 0, 0, 0}
	section := func(id byte, data []byte) {
		module = append(module, id)
		module = append(module, leb(len(data))...)
		module = append(module, data...)
	}
	section(1, []byte{2, 0x60, 4, 0x7f, 0x7f, 0x7f, 0x7f, 1, 0x7f, 0x60, 0, 0})
	imports := append([]byte{1}, wasmString("wasi_snapshot_preview1")...)
	imports = append(imports, wasmString("fd_write")...)
	imports = append(imports, 0, 0)
	section(2, imports)
	section(3, []byte{1, 1})
	section(5, []byte{1, 1, 1, 1})
	exports := append([]byte{2}, wasmString("_start")...)
	exports = append(exports, 0, 1)
	exports = append(exports, wasmString("memory")...)
	exports = append(exports, 2, 0)
	section(7, exports)
	code := []byte{0, 0x41, 1, 0x41, 0, 0x41, 1, 0x41, 8, 0x10, 0, 0x1a, 0x0b}
	if loop {
		code = []byte{0, 0x03, 0x40, 0x0c, 0, 0x0b, 0x0b}
	}
	body := append([]byte{1}, leb(len(code))...)
	body = append(body, code...)
	section(10, body)
	data := make([]byte, 16)
	binary.LittleEndian.PutUint32(data, 16)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(output)))
	data = append(data, []byte(output)...)
	segment := []byte{1, 0, 0x41, 0, 0x0b}
	segment = append(segment, leb(len(data))...)
	segment = append(segment, data...)
	section(11, segment)
	return module
}

func fixturePackage(t *testing.T, version, operation string) Package {
	t.Helper()
	p := Package{Content: Content{Manifest: Manifest{APIVersion: APIVersion, ID: "dev.synthetic.inspector", Name: "Synthetic inspector", Version: version, Permissions: []string{RuntimeRead}, Views: []View{{ID: "summary", Title: "Synthetic summary", Location: "tools"}}, Actions: []Action{{ID: "inspect", Title: "Inspect synthetic resource"}}}, UI: "<!doctype html><p>Synthetic isolated UI</p>", Module: fixtureModule(operation, false)}}
	p.SHA256 = Digest(p.Content)
	return p
}
func testManager(t *testing.T, broker RuntimeBroker) *Manager {
	t.Helper()
	m, err := Open(t.TempDir(), &testVault{}, broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return m
}

func TestExtensionLifecyclePermissionsAndTargetScope(t *testing.T) {
	var calls []string
	m := testManager(t, func(_ context.Context, environment, method, path string, _ json.RawMessage) (json.RawMessage, error) {
		calls = append(calls, environment+":"+method+":"+path)
		return json.RawMessage(`{"resource":"same-name"}`), nil
	})
	p := fixturePackage(t, "1.0.0", `{"capability":"runtime.read","method":"GET","path":"/api/docker/containers"}`)
	installed, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true})
	if err != nil {
		t.Fatal(err)
	}
	invocation := Invocation{Action: "inspect", EnvironmentID: "synthetic-a", Args: json.RawMessage(`{}`)}
	if _, err := m.Invoke(context.Background(), installed.ID, invocation); err == nil {
		t.Fatal("installed extension executed without approval")
	}
	if err := m.Configure(installed.ID, Configuration{Enabled: true, Approve: true, Grants: Grants{Permissions: []string{RuntimeRead}, Environments: []string{"synthetic-a"}}}); err != nil {
		t.Fatal(err)
	}
	result, err := m.Invoke(context.Background(), installed.ID, invocation)
	if err != nil || !strings.Contains(string(result.Data), "same-name") {
		t.Fatalf("invocation: %+v %v", result, err)
	}
	invocation.EnvironmentID = "synthetic-b"
	if _, err := m.Invoke(context.Background(), installed.ID, invocation); err == nil || len(calls) != 1 {
		t.Fatal("extension crossed its environment grant")
	}
	if err := m.Configure(installed.ID, Configuration{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	invocation.EnvironmentID = "synthetic-a"
	if _, err := m.Invoke(context.Background(), installed.ID, invocation); err == nil {
		t.Fatal("disabled extension retained authority")
	}
	if err := m.Remove(installed.ID); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatal("uninstalled extension remains")
	}
}

func TestPackageIntegrityProvenanceAndAtomicUpdate(t *testing.T) {
	m := testManager(t, nil)
	p := fixturePackage(t, "1.0.0", `{"capability":"result","data":{"synthetic":true}}`)
	changed := p
	changed.Content.UI += "altered"
	if _, err := m.Install(InstallRequest{Package: changed, ExpectedSHA256: p.SHA256, Developer: true}); err == nil {
		t.Fatal("altered artifact accepted")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.PublicKey = base64.StdEncoding.EncodeToString(public)
	p.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(p.SHA256)))
	if _, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256}); err == nil {
		t.Fatal("unknown publisher trusted implicitly")
	}
	if err := m.Trust(p.PublicKey, "Synthetic publisher"); err != nil {
		t.Fatal(err)
	}
	installed, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	bad := fixturePackage(t, "2.0.0", `{"capability":"result","data":{}}`)
	bad.Content.Module = []byte("not wasm")
	bad.SHA256 = Digest(bad.Content)
	bad.PublicKey = p.PublicKey
	bad.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(bad.SHA256)))
	if _, err := m.Install(InstallRequest{Package: bad, ExpectedSHA256: bad.SHA256}); err == nil {
		t.Fatal("invalid update committed")
	}
	if m.List()[0].Version != installed.Version {
		t.Fatal("failed update replaced active version")
	}
}

func TestSandboxTimeoutCancellationAndNoHostImports(t *testing.T) {
	m := testManager(t, nil)
	p := fixturePackage(t, "1.0.0", `{"capability":"secrets.read","secret":"porto-private"}`)
	p.Content.Manifest.Permissions = []string{}
	p.SHA256 = Digest(p.Content)
	installed, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(installed.ID, Configuration{Enabled: true, Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Invoke(context.Background(), installed.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("WASM obtained a secret without a grant")
	}
	p = fixturePackage(t, "2.0.0", `{}`)
	p.Content.Module = fixtureModule("", true)
	p.SHA256 = Digest(p.Content)
	if _, err := m.Install(InstallRequest{Package: p, ExpectedSHA256: p.SHA256, Developer: true}); err != nil {
		t.Fatal(err)
	}
	if err := m.Configure(installed.ID, Configuration{Enabled: true, Approve: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := m.Invoke(ctx, installed.ID, Invocation{Action: "inspect", EnvironmentID: "local"}); err == nil {
		t.Fatal("infinite module completed")
	}
	if time.Since(start) > time.Second {
		t.Fatal("canceled module retained CPU")
	}
}
