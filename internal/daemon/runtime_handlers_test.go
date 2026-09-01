package daemon

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func TestDockerContainerSnapshotReportsUnavailableInventory(t *testing.T) {
	server := &Server{docker: portodocker.New(nil)}
	response := httptest.NewRecorder()
	server.dockerContainerSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/docker/containers/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"available":false`) ||
		!strings.Contains(body, `"containers":[]`) ||
		!strings.Contains(body, `"directInventory":{"supported":true}`) {
		t.Fatalf("unexpected snapshot response: %s", body)
	}
}

func TestDockerContainerEventsSendsRevisionedSnapshot(t *testing.T) {
	server := &Server{docker: portodocker.New(nil)}
	response := httptest.NewRecorder()
	server.dockerContainerEvents(response, httptest.NewRequest(http.MethodGet, "/api/docker/containers/events", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q, want event stream", contentType)
	}
	body := response.Body.String()
	if !strings.Contains(body, "retry: 1000") ||
		!strings.Contains(body, "event: snapshot") ||
		!strings.Contains(body, `"revision":0`) {
		t.Fatalf("unexpected event stream: %s", body)
	}
}
