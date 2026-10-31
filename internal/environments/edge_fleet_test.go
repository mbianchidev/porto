package environments

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
)

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("synthetic asynchronous condition did not complete")
}

func TestEdgeOutboundReconnectRevocationAndStreamCleanup(t *testing.T) {
	manager := testManager(t)
	relay := httptest.NewTLSServer(manager.AgentHandler(http.NotFoundHandler()))
	t.Cleanup(relay.Close)
	invite, err := manager.Enrollment(EnrollmentRequest{Name: "synthetic-edge", Kind: "edge", Capabilities: []string{InventoryRead, StreamsRead, WorkloadOperate}, TTLSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}
	invite.Address, invite.Certificate = relay.URL, CertificatePEM(relay.Certificate())
	agent := testManager(t)
	streamStarted, streamClosed := make(chan struct{}), make(chan struct{})
	local := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/logs") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: synthetic\n\n")
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			close(streamStarted)
			<-r.Context().Done()
			close(streamClosed)
			return
		}
		JSON(w, 200, map[string]string{"host": "synthetic-edge", "resource": "same-name"})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- agent.RunEdge(ctx, &invite, local) }()
	var profile Profile
	eventually(t, func() bool {
		for _, p := range manager.List() {
			if p.Backend == "edge" && p.State == "online" {
				profile = p
				return true
			}
		}
		return false
	})
	firstRevision := profile.Revision
	manager.mu.Lock()
	err = manager.disconnectLocked(profile.ID)
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		for _, p := range manager.List() {
			if p.ID == profile.ID && p.State == "online" && p.Revision > firstRevision {
				return true
			}
		}
		return false
	})
	managerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { manager.Proxy(w, r, profile.ID) }))
	defer managerServer.Close()
	response, err := http.Get(managerServer.URL + "/api/docker/containers/same-name/logs")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	select {
	case <-streamStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("remote stream did not start")
	}
	if err := manager.RevokePeer(profile.Identity); err != nil {
		t.Fatal(err)
	}
	select {
	case <-streamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation retained a remote stream")
	}
	if _, err := io.ReadAll(response.Body); err != nil && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Logf("closed stream reported %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("revoked Edge identity silently succeeded")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("revoked Edge identity kept reconnecting")
	}
	if err := manager.Remove(profile.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(manager.List()) != 1 {
		t.Fatal("Edge uninstall retained target")
	}
}

func TestExternalKubernetesImportIdentityAndGlobalPreservation(t *testing.T) {
	manager := testManager(t)
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-cluster-credential" {
			http.Error(w, "synthetic authorization required", 401)
			return
		}
		JSON(w, 200, map[string]string{"gitVersion": "v1.37.1"})
	}))
	defer cluster.Close()
	global := filepath.Join(t.TempDir(), "unrelated-config")
	const unrelated = "synthetic unrelated global configuration\n"
	if err := os.WriteFile(global, []byte(unrelated), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", global)
	document := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: synthetic
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: observer
  user:
    token: synthetic-cluster-credential
contexts:
- name: imported
  context:
    cluster: synthetic
    user: observer
current-context: imported
`, cluster.URL, base64.StdEncoding.EncodeToString([]byte(CertificatePEM(cluster.Certificate()))))
	profile, err := manager.Add(context.Background(), AddRequest{Name: "external", Backend: "kubernetes", Context: "imported", Kubeconfig: document})
	if err != nil {
		t.Fatal(err)
	}
	if profile.Owned || len(profile.Capabilities) != 2 {
		t.Fatalf("import conferred ownership or mutation: %+v", profile)
	}
	provider := KubernetesConfig{Manager: manager, EnvironmentID: profile.ID}
	if _, _, err := provider.AdminConfig("unrelated-global"); err == nil {
		t.Fatal("global context used as fallback")
	}
	config, _, err := provider.AdminConfig("imported")
	if err != nil || config.BearerToken != "synthetic-cluster-credential" {
		t.Fatalf("imported identity: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(document, "token: synthetic-cluster-credential", "exec:\n      command: synthetic-untrusted-command\n      apiVersion: client.authentication.k8s.io/v1", 1),
		strings.Replace(document, "certificate-authority-data:", "certificate-authority:", 1),
		strings.Replace(document, "server: "+cluster.URL, "server: http://synthetic.invalid", 1),
		strings.Replace(document, "server: "+cluster.URL, "insecure-skip-tls-verify: true\n    server: "+cluster.URL, 1),
	} {
		if _, _, _, _, err := ImportKubeconfig(bad, "imported"); err == nil {
			t.Fatal("unsafe kubeconfig imported")
		}
	}
	data, err := os.ReadFile(global)
	if err != nil || string(data) != unrelated {
		t.Fatal("external import rewrote global kubeconfig")
	}
	if err := manager.Remove(profile.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.AdminConfig("imported"); err == nil {
		t.Fatal("removed imported identity remained usable")
	}
}

func TestFleetExactTargetsOfflineResultsConcurrentOwnershipAndRotation(t *testing.T) {
	manager := testManager(t)
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	manager.mu.Lock()
	for _, id := range []string{a, b} {
		manager.state.Profiles[id] = Profile{ID: id, Name: "same-name", Backend: "porto", Revision: 1, Capabilities: []string{InventoryRead, WorkloadOperate}}
	}
	manager.mu.Unlock()
	root := t.TempDir()
	ops := operations.New(filepath.Join(root, "operations"))
	defer ops.Close(context.Background())
	var mu sync.Mutex
	var actions []string
	release := make(chan struct{})
	request := func(ctx context.Context, id, method, path string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "GET" {
			return json.RawMessage(fmt.Sprintf(`{"Id":%q,"State":{"Status":"running"}}`, strings.Repeat(id[:1], 64))), nil
		}
		mu.Lock()
		actions = append(actions, id+":"+path)
		mu.Unlock()
		if id == b {
			return nil, problem(503, "offline", "synthetic offline; not queued")
		}
		select {
		case <-release:
			return json.RawMessage(`{}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fleet := NewFleet(filepath.Join(root, "fleet"), manager, ops, request)
	preview, err := fleet.Preview(context.Background(), FleetRequest{Action: "stop", Targets: []FleetTarget{{EnvironmentID: a, ResourceID: "same-name"}, {EnvironmentID: b, ResourceID: "same-name"}}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Targets[0].ResourceID == preview.Targets[1].ResourceID {
		t.Fatal("same-named resources collided")
	}
	if _, err := fleet.Start(context.Background(), preview.ID, "wrong"); err == nil {
		t.Fatal("destructive count confirmation skipped")
	}
	run, err := fleet.Start(context.Background(), preview.ID, preview.Confirmation)
	if err != nil {
		t.Fatal(err)
	}
	other, err := fleet.Preview(context.Background(), FleetRequest{Action: "restart", Targets: []FleetTarget{{EnvironmentID: a, ResourceID: "same-name"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fleet.Start(context.Background(), other.ID, other.Confirmation); err == nil {
		t.Fatal("concurrent fleet action stole active target")
	}
	close(release)
	eventually(t, func() bool { op, err := ops.Get(run.Operation); return err == nil && op.Status == "partial" })
	result, err := fleet.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Results[0].Status != "succeeded" || result.Results[1].Status != "failed" {
		t.Fatalf("per-target result lost: %+v", result)
	}
	mu.Lock()
	count := len(actions)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("offline action was replayed: %d dispatches", count)
	}
	if _, err := fleet.Start(context.Background(), preview.ID, preview.Confirmation); err == nil {
		t.Fatal("fleet preview replayed")
	}
	manager.mu.Lock()
	p := manager.state.Profiles[a]
	p.Revision++
	manager.state.Profiles[a] = p
	manager.mu.Unlock()
	next, err := fleet.Start(context.Background(), other.ID, other.Confirmation)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { op, err := ops.Get(next.Operation); return err == nil && op.Status == "partial" })
	mu.Lock()
	defer mu.Unlock()
	if len(actions) != count {
		t.Fatal("rotated environment identity accepted old preview")
	}
}

func TestFleetCancellationDoesNotQueueLaterTargets(t *testing.T) {
	manager := testManager(t)
	ops := operations.New(filepath.Join(t.TempDir(), "operations"))
	defer ops.Close(context.Background())
	started := make(chan struct{})
	request := func(ctx context.Context, _, method, _ string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "GET" {
			return json.RawMessage(`{"Id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`), nil
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	fleet := NewFleet(t.TempDir(), manager, ops, request)
	preview, err := fleet.Preview(context.Background(), FleetRequest{Action: "stop", Targets: []FleetTarget{{EnvironmentID: "local", ResourceID: "same-name"}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fleet.Start(context.Background(), preview.ID, preview.Confirmation)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("fleet mutation did not start")
	}
	if err := ops.Cancel(run.Operation); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		value, err := fleet.Get(run.ID)
		return err == nil && value.Results[0].Status == "canceled"
	})
}

func TestTLSMismatchMissingAuthorizationAndUnsupportedDockerVersions(t *testing.T) {
	manager := testManager(t)
	agent, server := testAgent(t, "synthetic")
	wrongCertificate, _, err := generateIdentity("127.0.0.1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	invite, err := agent.Enrollment(EnrollmentRequest{Name: "tls", Kind: "client", Capabilities: []string{InventoryRead}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Add(context.Background(), AddRequest{Name: "wrong-cert", Backend: "porto", Address: server.URL, Certificate: wrongCertificate, EnrollmentToken: invite.Token}); err == nil {
		t.Fatal("wrong TLS identity accepted")
	}
	response := httptest.NewRecorder()
	agent.AgentHandler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest("GET", "/v1/api/api/docker/containers", nil))
	if response.Code != 401 {
		t.Fatalf("unauthenticated network API accepted: %d", response.Code)
	}
	docker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		JSON(w, 200, map[string]string{"ApiVersion": "1.40", "MinAPIVersion": "1.39"})
	}))
	defer docker.Close()
	if _, err := manager.Add(context.Background(), AddRequest{Name: "old-docker", Backend: "docker", Address: docker.URL, Certificate: CertificatePEM(docker.Certificate()), Token: "synthetic"}); err == nil {
		t.Fatal("successful ping hid unsupported Docker version")
	}
	if err := validateCapabilities([]string{"unknown"}); err == nil {
		t.Fatal("unknown capability accepted")
	}
}
