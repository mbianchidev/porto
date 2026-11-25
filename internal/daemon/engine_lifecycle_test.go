package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	portodocker "github.com/porto-containers/porto/internal/docker"
	"github.com/porto-containers/porto/internal/runtimes"
)

type engineStartRunner struct {
	startContextErr error
	started         bool
}

func (r *engineStartRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	switch {
	case strings.Join(command.Args, " ") == "list porto-engine --json":
		return []byte(`{"name":"porto-engine","status":"Stopped"}`), nil
	case len(command.Args) > 0 && command.Args[0] == "start":
		r.started = true
		r.startContextErr = ctx.Err()
		return nil, errors.New("synthetic boot stops after recording its context")
	default:
		return nil, errors.New("unexpected synthetic command")
	}
}

func TestEngineStartOutlivesTheRequestThatAskedForIt(t *testing.T) {
	server, _, stateDir := engineProgressFixture(t, true, "limactl")
	runner := &engineStartRunner{}
	server.docker = portodocker.NewWithStateDir(runner, stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	server.startDockerEngine(response, httptest.NewRequest(http.MethodPost, "/api/docker/engine/start", nil).WithContext(ctx))
	if !runner.started {
		t.Fatalf("engine start was not attempted: %d %s", response.Code, response.Body.String())
	}
	if runner.startContextErr != nil {
		t.Fatalf("limactl start inherited the cancelled request context: %v", runner.startContextErr)
	}
}
