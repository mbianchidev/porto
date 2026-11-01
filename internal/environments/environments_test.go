package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/zalando/go-keyring"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryVault struct {
	mu     sync.Mutex
	values map[string]string
}

func (v *memoryVault) Set(service, account, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.values == nil {
		v.values = map[string]string{}
	}
	v.values[service+"/"+account] = secret
	return nil
}
func (v *memoryVault) Get(service, account string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (v *memoryVault) Delete(service, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, service+"/"+account)
	return nil
}

func testManager(t *testing.T) *Manager {
	t.Helper()
	m, err := Open(t.TempDir(), &memoryVault{})
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

func testAgent(t *testing.T, name string) (*Manager, *httptest.Server) {
	t.Helper()
	m := testManager(t)
	server := httptest.NewTLSServer(m.AgentHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"host":%q,"resource":"same-name","method":%q}`, name, r.Method)
	})))
	t.Cleanup(server.Close)
	return m, server
}

func enrollHost(t *testing.T, manager *Manager, agent *Manager, server *httptest.Server, name string) Profile {
	t.Helper()
	invite, err := agent.Enrollment(EnrollmentRequest{Name: name, Kind: "client", Capabilities: []string{InventoryRead, WorkloadOperate, StreamsRead}, TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := manager.Add(context.Background(), AddRequest{Name: name, Backend: "porto", Address: server.URL, Certificate: CertificatePEM(server.Certificate()), EnrollmentToken: invite.Token})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestExplicitTargetsDoNotCollideOrFallBack(t *testing.T) {
	manager := testManager(t)
	a, sa := testAgent(t, "synthetic-a")
	b, sb := testAgent(t, "synthetic-b")
	pa := enrollHost(t, manager, a, sa, "same-name")
	pb := enrollHost(t, manager, b, sb, "same-name")
	if pa.ID == pb.ID {
		t.Fatal("environment identities collided")
	}
	for _, target := range []struct {
		profile Profile
		host    string
	}{{pa, "synthetic-a"}, {pb, "synthetic-b"}} {
		r := httptest.NewRequest("GET", "/api/docker/containers/same-name/identity", nil)
		w := httptest.NewRecorder()
		manager.Proxy(w, r, target.profile.ID)
		if w.Code != 200 || !strings.Contains(w.Body.String(), target.host) || w.Header().Get("X-Porto-Environment-ID") != target.profile.ID {
			t.Fatalf("wrong target: %d %s %v", w.Code, w.Body, w.Header())
		}
	}
	w := httptest.NewRecorder()
	manager.Proxy(w, httptest.NewRequest("POST", "/api/docker/containers/same-name/start", nil), "unknown")
	if w.Code != 404 {
		t.Fatalf("unknown target fell through: %d", w.Code)
	}
	sa.Close()
	w = httptest.NewRecorder()
	manager.Proxy(w, httptest.NewRequest("POST", "/api/docker/containers/same-name/start", nil), pa.ID)
	if w.Code != 503 {
		t.Fatalf("offline mutation was queued or redirected: %d", w.Code)
	}
}

func TestEnrollmentIdentityPermissionsRotationAndRevocation(t *testing.T) {
	manager := testManager(t)
	agent, server := testAgent(t, "synthetic")
	invite, err := agent.Enrollment(EnrollmentRequest{Name: "observer", Kind: "client", Capabilities: []string{InventoryRead}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	bad := AddRequest{Name: "observer", Backend: "porto", Address: server.URL, Certificate: CertificatePEM(server.Certificate()), EnrollmentToken: invite.Token}
	bad.Certificate = ""
	if _, err := manager.Add(context.Background(), bad); err == nil {
		t.Fatal("missing server identity accepted")
	}
	bad.Certificate = CertificatePEM(server.Certificate())
	profile, err := manager.Add(context.Background(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), bad); err == nil {
		t.Fatal("enrollment reused")
	}
	w := httptest.NewRecorder()
	manager.Proxy(w, httptest.NewRequest("POST", "/api/docker/containers/same-name/stop", nil), profile.ID)
	if w.Code != 403 {
		t.Fatalf("observe identity mutated workload: %d", w.Code)
	}
	if err := manager.Rotate(context.Background(), profile.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	manager.Proxy(w, httptest.NewRequest("GET", "/api/docker/containers", nil), profile.ID)
	if w.Code != 200 {
		t.Fatalf("rotated identity unavailable: %d %s", w.Code, w.Body)
	}
	if err := manager.Revoke(context.Background(), profile.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	manager.Proxy(w, httptest.NewRequest("GET", "/api/docker/containers", nil), profile.ID)
	if w.Code != 401 {
		t.Fatalf("revoked credential accepted: %d", w.Code)
	}
}

func TestExpiredEnrollmentAndUnsafeRoutesFailClosed(t *testing.T) {
	agent := testManager(t)
	invite, err := agent.Enrollment(EnrollmentRequest{Name: "expired", Kind: "client", Capabilities: []string{InventoryRead}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	agent.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	body, _ := json.Marshal(map[string]string{"token": invite.Token, "kind": "client"})
	w := httptest.NewRecorder()
	agent.AgentHandler(http.NotFoundHandler()).ServeHTTP(w, httptest.NewRequest("POST", "/v1/enroll", strings.NewReader(string(body))))
	if w.Code != 401 {
		t.Fatalf("expired enrollment: %d", w.Code)
	}
	for _, path := range []string{"/api/environments", "/api/extensions", "/api/registries", "/api/scan", "/api/../settings", "/api/docker/engine/remove"} {
		required, err := RequiredCapability("POST", path, nil)
		if err == nil && required != InfrastructureOperate {
			t.Fatalf("unsafe route exposed: %s (%s)", path, required)
		}
	}
}
