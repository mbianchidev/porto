package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/kubernetes"
)

func TestKubernetesUpgradeRequiresExplicitConfirmation(t *testing.T) {
	for _, action := range []string{"upgrade", "upgrade-recover"} {
		t.Run(action, func(t *testing.T) {
			server := &Server{}
			request := httptest.NewRequest(http.MethodPost, "/api/kubernetes/clusters/dev/"+action, strings.NewReader(`{"confirm":false}`))
			response := httptest.NewRecorder()
			if action == "upgrade" {
				server.upgradeKubernetesCluster(response, request)
			} else {
				server.recoverKubernetesUpgrade(response, request)
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "confirm=true") {
				t.Fatalf("response = %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestKubernetesUpgradeRejectsConcurrentMutation(t *testing.T) {
	server := &Server{clusters: kubernetes.NewClusterProvisioner(nil, nil, t.TempDir()), runtimeContext: context.Background()}
	release, err := server.beginKubernetesClusterOperation(context.Background(), "dev", "scale")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	request := httptest.NewRequest(http.MethodPost, "/api/kubernetes/clusters/dev/upgrade", strings.NewReader(`{"version":"v1.36.1+k3s1","confirm":true}`))
	request.SetPathValue("name", "dev")
	response := httptest.NewRecorder()
	server.upgradeKubernetesCluster(response, request)
	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "operation scale is already in progress") {
		t.Fatalf("response = %d: %s", response.Code, response.Body.String())
	}
}
