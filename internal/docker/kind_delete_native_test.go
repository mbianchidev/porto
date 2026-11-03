package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

type ownedKindDeleteRunner struct{ endpoint string }

func (r ownedKindDeleteRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	if command.Name == "kind" || command.Name == "docker" {
		command.Env = append(command.Env, "DOCKER_HOST="+r.endpoint, "DOCKER_CONTEXT=")
	}
	return (runtimes.ExecRunner{}).Run(ctx, command)
}

func TestNativeOwnedKindDeletion(t *testing.T) {
	name, root := os.Getenv("PORTO_TEST_KIND_DELETE_NAME"), os.Getenv("PORTO_TEST_KIND_DELETE_ROOT")
	if name == "" && root == "" {
		t.Skip("set PORTO_TEST_KIND_DELETE_NAME and PORTO_TEST_KIND_DELETE_ROOT to remove an explicitly owned disposable kind fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("the native deletion fixture uses an isolated Unix Docker socket")
	}
	if !strings.HasPrefix(name, "porto-gitops-acceptance-") || !filepath.IsAbs(root) || os.Getenv("PORTO_TEST_KIND_DELETE_CONFIRM") != "owned-fixture" {
		t.Fatal("native deletion requires explicit owned-fixture confirmation, an absolute metadata root and a porto-gitops-acceptance-* cluster")
	}
	filename := filepath.Join(root, config.KubernetesClusterFileToken(name)+".json")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var request kubernetes.ClusterRequest
	if json.Unmarshal(data, &request) != nil || request.Name != name || request.Provider != "kind" {
		t.Fatal("the selected disposable metadata does not identify this exact kind cluster")
	}
	socketDir, err := os.MkdirTemp("/tmp", "porto-kind-owned-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(socketDir); err != nil {
			t.Error(err)
		}
	})
	socketPath := filepath.Join(socketDir, "docker.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	manager := New(nil)
	runner := ownedKindDeleteRunner{endpoint: EndpointURL(socketPath)}
	provisioner := kubernetes.NewClusterProvisioner(vm.New(runner), runner, root)
	manager.SetManagedContainerGuard(provisioner.ProtectContainerRemoval)
	liveSocket, err := config.DockerSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", liveSocket)
	}}
	defer transport.CloseIdleConnections()
	readAPI := &httputil.ReverseProxy{Director: func(request *http.Request) {
		request.URL.Scheme, request.URL.Host = "http", "localhost"
		request.Header.Del(runtimes.ManagedContainerRemovalHeader)
	}, Transport: transport}
	deleteAPI := NewAPI(manager, socketPath)
	// Preserve the installed daemon's read-side runtime contract while exercising new deletion code.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteAPI.ServeHTTP(w, r)
		} else {
			readAPI.ServeHTTP(w, r)
		}
	})
	server := NewAPIServer(socketPath, handler)
	if err := server.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	if err := provisioner.Delete(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful native deletion retained ownership: %v", err)
	}
}
