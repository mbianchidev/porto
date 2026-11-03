package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mbianchidev/porto/internal/kubeadmin"
	"github.com/mbianchidev/porto/internal/operations"
)

func TestGitOpsRoutesRequireExplicitScopeAndDaemonMutationContext(t *testing.T) {
	ops := operations.New(t.TempDir())
	server := &Server{externalKubernetes: true, kubeAdmin: kubeadmin.New(t.TempDir(), nil, ops, &managementVault{values: map[string]string{}}, nil), runtimeContext: context.Background()}
	mux := http.NewServeMux()
	server.kubernetesAdminRoutes(mux)
	for _, entry := range []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/kubernetes/gitops/controller", "", 400},
		{"GET", "/api/kubernetes/gitops/sources", "", 400},
		{"POST", "/api/kubernetes/gitops/previews", `{}`, 400},
		{"POST", "/api/kubernetes/gitops/controller/previews", `{}`, 400},
		{"POST", "/api/kubernetes/gitops/apply", `{}`, 400},
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(entry.method, entry.path, bytes.NewBufferString(entry.body)))
		if response.Code != entry.status {
			t.Fatalf("%s %s: %d %s", entry.method, entry.path, response.Code, response.Body.String())
		}
	}
	server.runtimeContext = nil
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/kubernetes/gitops/apply", bytes.NewBufferString(`{"confirm":true}`)))
	if response.Code != 503 {
		t.Fatalf("missing daemon context: %d %s", response.Code, response.Body.String())
	}
}
