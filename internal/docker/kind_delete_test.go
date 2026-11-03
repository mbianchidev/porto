package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestManagedKindDeletionCrossesDockerAPIWithoutUnprotectingOtherRequests(t *testing.T) {
	root := t.TempDir()
	writeOwner := func(name string) {
		document, err := json.Marshal(kubernetes.ClusterRequest{Name: name, Provider: "kind"})
		if err != nil {
			t.Fatal(err)
		}
		if err := config.WriteFileAtomic(filepath.Join(root, config.KubernetesClusterFileToken(name)+".json"), document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOwner("synthetic")
	writeOwner("unrelated")
	removed := false
	dockerRunner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if len(command.Args) >= 3 && command.Args[0] == "container" && command.Args[1] == "inspect" {
			name := command.Args[2]
			return []byte(fmt.Sprintf(`[{"Id":"%s-id","Name":"/%s"}]`, name, name)), nil
		}
		if len(command.Args) > 0 && command.Args[0] == "rm" {
			removed = true
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected synthetic Docker command: %s %v", command.Name, command.Args)
	}}
	manager := New(dockerRunner)
	var handler http.Handler
	kindRunner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "kind" {
			return nil, fmt.Errorf("unexpected kind fixture command %s", command.Name)
		}
		token := ""
		for _, entry := range command.Env {
			if value, ok := strings.CutPrefix(entry, "DOCKER_CUSTOM_HEADERS="+runtimes.ManagedContainerRemovalHeader+"="); ok {
				token = value
			}
		}
		if token == "" {
			return nil, fmt.Errorf("kind subprocess has no deletion authorization")
		}
		for _, entry := range []struct {
			name    string
			allowed bool
		}{{"porto-unrelated-control-plane", false}, {"porto-synthetic-control-plane", true}} {
			request := httptest.NewRequest(http.MethodDelete, "/v1.47/containers/"+entry.name+"?force=1&v=1", nil)
			request.Header.Set(runtimes.ManagedContainerRemovalHeader, token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if entry.allowed && response.Code != http.StatusNoContent || !entry.allowed && response.Code < 400 {
				return nil, fmt.Errorf("scoped deletion for %s: %d %s", entry.name, response.Code, response.Body.String())
			}
		}
		return nil, nil
	}}
	provisioner := kubernetes.NewClusterProvisioner(vm.New(kindRunner), kindRunner, root)
	manager.SetManagedContainerGuard(provisioner.ProtectContainerRemoval)
	handler = NewAPI(manager, "")
	for _, token := range []string{"", "synthetic-forged-token"} {
		request := httptest.NewRequest(http.MethodDelete, "/v1.47/containers/porto-synthetic-control-plane?force=1&v=1", nil)
		request.Header.Set(runtimes.ManagedContainerRemovalHeader, token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 || removed {
			t.Fatalf("direct/forged Docker request bypassed managed protection: %d %s", response.Code, response.Body.String())
		}
	}
	if err := provisioner.Delete(context.Background(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("the owning cluster deletion did not remove its control plane")
	}
}
