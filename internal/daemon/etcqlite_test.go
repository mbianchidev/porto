package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/etcqlite"
	"github.com/mbianchidev/porto/internal/store"
)

func etcqliteTestServer(t *testing.T) (*Server, *http.ServeMux, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root := filepath.Join(t.TempDir(), "untouched-etcqlite")
	server := &Server{store: st, etcqlite: etcqlite.New(root, nil), runtimeContext: context.Background()}
	mux := http.NewServeMux()
	server.etcqliteRoutes(mux)
	return server, mux, root
}

func TestEtcqliteHTTPDisabledGatePrecedesDecodingAndAllSideEffects(t *testing.T) {
	_, mux, root := etcqliteTestServer(t)
	for _, route := range []struct{ method, path string }{
		{"POST", "/instances"}, {"POST", "/instances/synthetic/start"},
		{"POST", "/instances/synthetic/stop"}, {"POST", "/instances/synthetic/backup"},
		{"POST", "/instances/synthetic/restore"}, {"DELETE", "/instances/synthetic"},
		{"GET", "/backups"}, {"GET", "/instances/synthetic/logs"},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(route.method, "/api/integrations/etcqlite"+route.path, strings.NewReader("invalid JSON")))
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "disabled") {
			t.Fatalf("disabled route %s: %d %s", route.path, response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/api/integrations/etcqlite", nil))
	var status etcqlite.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil || status.State != "disabled" || len(status.Instances) != 0 {
		t.Fatalf("disabled inventory: %+v %v", status, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("disabled HTTP operations created or read native state: %v", err)
	}
}

func TestEtcqliteEnableDoesNotInitializeStartOrChangeOtherSettings(t *testing.T) {
	if !etcqlite.Supported() {
		t.Skip("macOS/Linux native profile")
	}
	server, mux, root := etcqliteTestServer(t)
	before, err := server.store.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/integrations/etcqlite/enable", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	after, err := server.store.Settings(context.Background())
	if err != nil || !after.EtcqliteEnabled || after.DockerEnabled != before.DockerEnabled || after.KubernetesEnabled != before.KubernetesEnabled {
		t.Fatalf("enable changed unrelated defaults: %+v %v", after, err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("enable performed native filesystem work: %v", err)
	}
	for _, body := range []string{`{"name":"../foreign"}`, `{"name":"synthetic","members":2}`, `{"name":"synthetic","unknown":true}`} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/integrations/etcqlite/instances", bytes.NewBufferString(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid native request accepted: %d %s", response.Code, response.Body.String())
		}
	}
	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/integrations/etcqlite/instances/synthetic/start", strings.NewReader("{}")))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "clusterID") {
		t.Fatalf("name-only mutation accepted: %d %s", response.Code, response.Body.String())
	}
}
