package daemon

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
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

func TestCreateDockerContainerRunsLocalOrRemoteImage(t *testing.T) {
	var commands []string
	runner := runtimeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		commands = append(commands, args)
		switch {
		case strings.HasPrefix(args, "create "):
			return []byte("container-id\n"), nil
		case args == "start container-id":
			return nil, nil
		default:
			return nil, nil
		}
	})
	server := &Server{docker: portodocker.New(runner)}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/docker/containers",
		bytes.NewBufferString(`{
			"name":"web",
			"image":"nginx:alpine",
			"hostPort":8080,
			"containerPort":80,
			"healthCommand":"wget -q -O /dev/null http://127.0.0.1/"
		}`),
	)
	server.createDockerContainer(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create response = %d: %s", response.Code, response.Body.String())
	}
	joined := strings.Join(commands, "\n")
	for _, expected := range []string{
		"create --name web",
		"--health-cmd wget -q -O /dev/null http://127.0.0.1/",
		"--publish 127.0.0.1:8080:80/tcp nginx:alpine",
		"start container-id",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, joined)
		}
	}
}
