package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestInspectorFilesRejectInvalidPathsBeforeAccessingRuntime(t *testing.T) {
	for _, relative := range []string{"../outside", "/absolute", `..\outside`, "fixture\x00text"} {
		t.Run(url.QueryEscape(relative), func(t *testing.T) {
			var commands int
			server := &Server{docker: portodocker.New(runtimeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) {
				commands++
				return nil, errors.New("synthetic backend should not be accessed")
			}))}
			request := httptest.NewRequest(http.MethodGet, "/api/docker/containers/synthetic/files?"+
				url.Values{"path": {relative}}.Encode(), nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			request = request.WithContext(ctx)
			request.SetPathValue("id", "synthetic")
			response := httptest.NewRecorder()
			server.dockerFiles("container", "list")(response, request)
			if response.Code != http.StatusBadRequest || commands != 0 {
				t.Fatalf("unsafe path reached runtime: status=%d, commands=%d, body=%s",
					response.Code, commands, response.Body.String())
			}
		})
	}
}
