package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestDockerReadOnlyIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_INTEGRATION=1 to test a live Docker endpoint")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := New(nil)
	status := manager.Status(ctx, "")
	if !status.Available {
		t.Fatalf("Porto container runtime unavailable: %s", status.Message)
	}
	if _, err := manager.Containers(ctx); err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if _, err := manager.Images(ctx); err != nil {
		t.Fatalf("list images: %v", err)
	}
	if _, err := manager.Networks(ctx); err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if _, err := manager.Volumes(ctx); err != nil {
		t.Fatalf("list volumes: %v", err)
	}
}

func TestKindClusterIntegration(t *testing.T) {
	if os.Getenv("PORTO_KIND_INTEGRATION") != "1" {
		t.Skip("set PORTO_KIND_INTEGRATION=1 to test KinD against the Porto Docker API")
	}
	if _, err := exec.LookPath("kind"); err != nil {
		t.Skip("kind is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	portoHome, err := os.MkdirTemp("/tmp", "porto-kind-home-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(portoHome) })
	t.Setenv("PORTO_HOME", portoHome)
	engineDirectory, err := config.DockerEngineDir()
	if err != nil {
		t.Fatal(err)
	}
	manager := NewWithStateDir(nil, engineDirectory)
	if _, err := manager.InstallEngine(ctx); err != nil {
		t.Fatalf("install Porto engine: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		_ = manager.RemoveEngine(cleanupContext)
	})

	socketPath, err := config.DockerSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	server := NewAPIServer(socketPath, NewAPI(manager, socketPath))
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start Porto Docker API: %v", err)
	}
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	clusterName := "api-smoke"
	provisioner := kubernetes.NewClusterProvisioner(vm.New(nil), nil, t.TempDir())
	cluster, err := provisioner.Create(ctx, kubernetes.ClusterRequest{
		Name:         clusterName,
		Provider:     "kind",
		ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
	})
	if err != nil {
		t.Fatalf("create Porto KinD cluster: %v", err)
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cleanupCancel()
		_ = provisioner.Delete(cleanupContext, clusterName)
	})
	if cluster.Provider != "kind" || !strings.Contains(strings.Join(cluster.Nodes, " "), "porto-"+clusterName+"-control-plane") {
		t.Fatalf("unexpected KinD cluster: %+v", cluster)
	}
	dockerEnvironment := append(os.Environ(), "DOCKER_HOST="+EndpointURL(socketPath), "DOCKER_CONTEXT=")
	pull := exec.CommandContext(ctx, "docker", "pull", "alpine:3.20")
	pull.Env = dockerEnvironment
	if output, err := pull.CombinedOutput(); err != nil {
		t.Fatalf("pull KinD test image: %v: %s", err, output)
	}
	load := exec.CommandContext(ctx, "kind", "load", "docker-image", "alpine:3.20", "--name", "porto-"+clusterName)
	load.Env = dockerEnvironment
	if output, err := load.CombinedOutput(); err != nil {
		t.Fatalf("load image into KinD: %v: %s", err, output)
	}
}
