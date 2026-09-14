package daemon

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestClusterCreationBudgetIncludesRuntimeAndAddonStages(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	synctest.Test(t, func(t *testing.T) {
		gatewayReached := false
		runner := runtimeRunnerFunc(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
			args := strings.Join(command.Args, " ")
			var duration time.Duration
			switch {
			case command.Name == "kind" && strings.HasPrefix(args, "create cluster"):
				duration = 11 * time.Minute
			case command.Name == "kubectl" && (strings.Contains(args, "wait --for=condition=Ready nodes") ||
				strings.Contains(args, "wait --for=condition=Available deployment/metrics-server") ||
				strings.Contains(args, "wait --for=condition=Available apiservice/v1beta1.metrics")):
				duration = 4 * time.Minute
			case command.Name == "kubectl" && strings.Contains(args, "config view --raw -o json"):
				return []byte(`{"apiVersion":"v1","kind":"Config","current-context":"test","clusters":[{"name":"test","cluster":{"server":"https://127.0.0.1:54321"}}],"contexts":[{"name":"test","context":{"cluster":"test","user":"test"}}],"users":[{"name":"test","user":{"token":"synthetic-token"}}]}`), nil
			case command.Name == "kubectl" && strings.Contains(args, "get storageclass"):
				return []byte(`{"items":[{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}`), nil
			case command.Name == "kubectl" && strings.Contains(args, "envoyproxy/gateway/releases"):
				gatewayReached = true
				return nil, errors.New("synthetic gateway stop")
			}
			if duration > 0 {
				timer := time.NewTimer(duration)
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-ctx.Done():
					return []byte("synthetic setup progress"), errors.New("signal: killed")
				}
			}
			return nil, nil
		})
		provisioner := kubernetes.NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
		provisioner.SetRegistryConfigProvider(func(context.Context) ([]byte, error) {
			return []byte(`{"auths":{}}`), nil
		})
		server := &Server{clusters: provisioner, runtimeContext: context.Background()}
		request := httptest.NewRequest(http.MethodPost, "/api/kubernetes/clusters", bytes.NewBufferString(
			`{"name":"test-kind","provider":"kind","version":"v1.37.0","nodeGroups":[{"name":"workers","count":1}]}`,
		))
		response := httptest.NewRecorder()
		started := time.Now()

		server.createKubernetesCluster(response, request)

		if !gatewayReached || !strings.Contains(response.Body.String(), "synthetic gateway stop") {
			t.Fatalf("overall creation deadline cut off healthy stages: %s", response.Body.String())
		}
		if elapsed := time.Since(started); elapsed < 23*time.Minute {
			t.Fatalf("creation stages did not complete: %s", elapsed)
		}
	})
}
