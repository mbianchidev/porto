package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mbianchidev/porto/internal/environments"
	"github.com/mbianchidev/porto/internal/extensions"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/zalando/go-keyring"
)

type managementVault struct {
	mu     sync.Mutex
	values map[string]string
}

func (v *managementVault) Set(service, account, value string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[service+"/"+account] = value
	return nil
}
func (v *managementVault) Get(service, account string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (v *managementVault) Delete(service, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, service+"/"+account)
	return nil
}

func managementServer(t *testing.T) (*Server, http.Handler, *managementVault) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, nil)
	if s.extensions != nil {
		s.extensions.Close(context.Background())
	}
	if s.environments != nil {
		s.environments.Close(context.Background())
	}
	vault := &managementVault{values: map[string]string{}}
	s.managementVault = vault
	s.environments, s.environmentsErr = environments.Open(s.managementRoot, vault)
	s.extensions, s.extensionsErr = extensions.Open(filepath.Join(st.Directory(), "extensions"), vault, s.targetJSON)
	s.runtimeContext = context.Background()
	s.fleet = environments.NewFleet(filepath.Join(s.managementRoot, "fleet"), s.environments, s.operations, s.targetJSON)
	if s.environmentsErr != nil || s.extensionsErr != nil {
		t.Fatalf("open synthetic management: %v %v", s.environmentsErr, s.extensionsErr)
	}
	t.Cleanup(func() {
		if err := s.extensions.Close(context.Background()); err != nil {
			t.Error(err)
		}
		if err := s.environments.Close(context.Background()); err != nil {
			t.Error(err)
		}
		for _, scope := range s.environmentKube {
			if err := scope.operations.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}
		if err := s.operations.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	mux := http.NewServeMux()
	s.routes(mux)
	return s, s.secureHandler(mux), vault
}
func managementCall(handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:37623"+path, bytes.NewReader(data))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestEnvironmentAPIUnknownTargetsNeverReachLocalResources(t *testing.T) {
	_, handler, _ := managementServer(t)
	for _, path := range []string{
		"/api/environments/" + strings.Repeat("a", 32) + "/api/docker/containers/same-name/stop",
		"/api/environments/" + strings.Repeat("b", 32) + "/api/projects/same-name/start",
	} {
		response := managementCall(handler, "POST", path, nil)
		if response.Code != 404 {
			t.Fatalf("unknown target routed locally: %d %s", response.Code, response.Body)
		}
	}
	request := httptest.NewRequest("POST", "http://127.0.0.1/api/projects/synthetic/start", nil)
	request.Header.Set("X-Porto-Environment-ID", strings.Repeat("a", 32))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatal("unscoped remote header silently routed locally")
	}
}

func TestImportedClusterUsesOnlyItsIdentityAndNeverOwnsInfrastructure(t *testing.T) {
	server, handler, _ := managementServer(t)
	var deletes atomic.Int32
	cluster := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-import-credential" {
			http.Error(w, "synthetic authentication required", 401)
			return
		}
		if r.Method == "DELETE" {
			deletes.Add(1)
			t.Error("imported infrastructure received deletion")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.37.1"}`)
		case "/api":
			fmt.Fprint(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[{"name":"pods","kind":"Pod","namespaced":true,"verbs":["get","list"]}]}`)
		case "/api/v1/namespaces/default/pods":
			fmt.Fprint(w, `{"kind":"PodList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"default","name":"same-name","uid":"synthetic-imported-uid","resourceVersion":"1"}}]}`)
		case "/api/v1/namespaces/default/pods/same-name":
			fmt.Fprint(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"default","name":"same-name","uid":"synthetic-imported-uid","resourceVersion":"1"},"status":{"phase":"Running"}}`)
		case "/api/v1/namespaces/default/pods/same-name/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "synthetic imported pod logs")
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			fmt.Fprint(w, `{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview","status":{"allowed":true}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer cluster.Close()
	directory := t.TempDir()
	global := filepath.Join(directory, "unrelated-kubeconfig")
	if err := os.WriteFile(global, []byte("synthetic untouched global config"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", global)
	kubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: imported-observer
  user:
    token: synthetic-import-credential
contexts:
- name: explicit-import
  context:
    cluster: cluster
    user: imported-observer
current-context: explicit-import
`, cluster.URL, base64.StdEncoding.EncodeToString([]byte(environments.CertificatePEM(cluster.Certificate()))))
	added := managementCall(handler, "POST", "/api/environments", environments.AddRequest{Name: "synthetic external", Backend: "kubernetes", Context: "explicit-import", Kubeconfig: kubeconfig})
	if added.Code != 201 {
		t.Fatalf("import: %d %s", added.Code, added.Body)
	}
	if strings.Contains(added.Body.String(), "synthetic-import-credential") {
		t.Fatal("import response leaked credentials")
	}
	var profile environments.Profile
	if err := json.Unmarshal(added.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	base := "/api/environments/" + profile.ID
	response := managementCall(handler, "GET", base+"/api/kubernetes/status?context=explicit-import", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "v1.37.1") {
		t.Fatalf("imported status: %d %s", response.Code, response.Body)
	}
	query := url.Values{"context": {"explicit-import"}, "namespace": {"default"}, "version": {"v1"}, "resource": {"pods"}}
	response = managementCall(handler, "GET", base+"/api/kubernetes/admin/resources?"+query.Encode(), nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "synthetic-imported-uid") {
		t.Fatalf("native imported inventory: %d %s", response.Code, response.Body)
	}
	response = managementCall(handler, "GET", base+"/api/kubernetes/pods/default/same-name/logs?context=explicit-import&uid=synthetic-imported-uid", nil)
	if response.Code != 200 || response.Body.String() != "synthetic imported pod logs" {
		t.Fatalf("imported pod logs: %d %s", response.Code, response.Body)
	}
	response = managementCall(handler, "GET", base+"/api/kubernetes/pods/default/same-name/logs?context=explicit-import&uid=synthetic-recreated-uid", nil)
	if response.Code != 409 {
		t.Fatalf("recreated pod stream was retargeted: %d %s", response.Code, response.Body)
	}
	response = managementCall(handler, "GET", base+"/api/kubernetes/admin/resources?context=unrelated-global&namespace=default&version=v1&resource=pods", nil)
	if response.Code != 403 {
		t.Fatalf("wrong context was used: %d %s", response.Code, response.Body)
	}
	for _, path := range []string{"/api/kubernetes/clusters/owned", "/api/kubernetes/admin/actions"} {
		response = managementCall(handler, "POST", base+path, map[string]any{"context": "explicit-import", "action": "delete", "confirm": true})
		if response.Code != 403 {
			t.Fatalf("observe import mutated cluster: %s %d", path, response.Code)
		}
	}
	response = managementCall(handler, "DELETE", base, nil)
	if response.Code != 204 {
		t.Fatalf("remove imported environment: %d %s", response.Code, response.Body)
	}
	if deletes.Load() != 0 {
		t.Fatal("uninstall deleted remote workloads")
	}
	if _, err := os.Stat(filepath.Join(server.managementRoot, "kubernetes", profile.ID)); !os.IsNotExist(err) {
		t.Fatalf("imported administration state retained: %v", err)
	}
	data, err := os.ReadFile(global)
	if err != nil || string(data) != "synthetic untouched global config" {
		t.Fatal("global kubeconfig changed")
	}
}

func TestManagementDecoderRejectsUnknownFieldsAndRemoteExtensionAccess(t *testing.T) {
	server, handler, _ := managementServer(t)
	response := managementCall(handler, "POST", "/api/extensions", map[string]any{"unrestrictedHostPrivileges": true})
	if response.Code != 400 {
		t.Fatalf("unknown extension privilege accepted: %d", response.Code)
	}
	agent := server.environments.AgentHandler(server.coreHandler)
	invite, err := server.environments.Enrollment(environments.EnrollmentRequest{Name: "synthetic", Kind: "client", Capabilities: environments.AllCapabilities, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	enrolled := managementCall(agent, "POST", "/v1/enroll", map[string]string{"token": invite.Token, "kind": "client"})
	var identity struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(enrolled.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "https://synthetic.invalid/v1/api/api/extensions", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+identity.Token)
	recorder := httptest.NewRecorder()
	agent.ServeHTTP(recorder, request)
	if recorder.Code != 403 {
		t.Fatalf("remote full-capability identity reached extension host: %d %s", recorder.Code, recorder.Body)
	}
}
