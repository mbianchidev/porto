package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

func TestContainerInventoryIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_INTEGRATION=1 to test the live containerd inventory")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := New(nil)
	if err := manager.StartContainerInventory(ctx); err != nil {
		t.Fatalf("start container inventory: %v", err)
	}
	t.Cleanup(func() {
		stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = manager.StopContainerInventory(stopContext)
	})
	updates, unsubscribe := manager.SubscribeContainerSnapshots()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("container inventory did not become available: %+v", manager.ContainerSnapshot())
		case snapshot, ok := <-updates:
			if !ok {
				t.Fatal("container inventory subscription closed before becoming available")
			}
			if snapshot.Available {
				if snapshot.Namespace == "" || snapshot.InstanceID == "" {
					t.Fatalf("inventory identity is incomplete: %+v", snapshot)
				}
				return
			}
		}
	}
}

func TestContainerInventoryExternalLifecycleIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_INTEGRATION=1 to test external container lifecycle events")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	manager := New(nil)
	if err := manager.StartContainerInventory(ctx); err != nil {
		t.Fatalf("start container inventory: %v", err)
	}
	t.Cleanup(func() {
		stopContext, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = manager.StopContainerInventory(stopContext)
	})
	updates, unsubscribe := manager.SubscribeContainerSnapshots()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("container inventory did not become available: %+v", manager.ContainerSnapshot())
		case snapshot, ok := <-updates:
			if !ok {
				t.Fatal("container inventory subscription closed")
			}
			if snapshot.Available {
				goto inventoryReady
			}
		}
	}

inventoryReady:
	backend, err := manager.backend(ctx)
	if err != nil {
		t.Fatalf("resolve backend: %v", err)
	}
	name := fmt.Sprintf("porto-inventory-test-%d", time.Now().UnixNano())
	runExternal := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, backend.name, append(append([]string(nil), backend.prefix...), args...)...)
		return command.CombinedOutput()
	}
	output, err := runExternal("create", "--name", name, "alpine:latest", "sleep", "30")
	if err != nil {
		t.Fatalf("create external container: %v: %s", err, output)
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		t.Fatalf("external container create returned no identifier: %s", output)
	}
	id := fields[len(fields)-1]
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		command := exec.CommandContext(
			cleanupContext,
			backend.name,
			append(append([]string(nil), backend.prefix...), "rm", "--force", id)...,
		)
		_ = command.Run()
	})

	startedAt := time.Now()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("external container was not observed: %+v", manager.ContainerSnapshot())
		case snapshot, ok := <-updates:
			if !ok {
				t.Fatal("container inventory subscription closed")
			}
			for _, container := range snapshot.Containers {
				if container.ID == id || container.Name == name {
					if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
						t.Fatalf("external container discovery took %s", elapsed)
					}
					return
				}
			}
		}
	}
}

func TestDockerComposeIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_INTEGRATION=1 to test Docker Compose against the live Porto backend")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI is not installed")
	}
	if output, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("Docker Compose is not available: %v: %s", err, output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	manager := New(nil)
	status := manager.Status(ctx, "")
	if !status.Available {
		t.Fatalf("Porto container runtime unavailable: %s", status.Message)
	}
	if err := manager.PullImage(ctx, "alpine:latest", ""); err != nil {
		t.Fatalf("pull test image: %v", err)
	}

	socketDir, err := os.MkdirTemp("/tmp", "porto-compose-integration-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "docker.sock")
	server := NewAPIServer(socketPath, NewAPI(manager, socketPath))
	if err := server.Start(ctx); err != nil {
		t.Fatalf("start Porto Docker API: %v", err)
	}
	t.Cleanup(func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = server.Close(closeContext)
	})

	composeFile := filepath.Join(socketDir, "compose.yaml")
	if err := os.WriteFile(composeFile, []byte(`services:
  server:
    image: alpine:latest
    command: ["sleep", "30"]
    healthcheck:
      test: ["CMD", "true"]
      interval: 1s
      timeout: 1s
      retries: 3
  client:
    image: alpine:latest
    command: ["sh", "-c", "getent hosts server && sleep 30"]
    depends_on:
      server:
        condition: service_healthy
`), 0o600); err != nil {
		t.Fatal(err)
	}
	projectName := fmt.Sprintf("porto-compose-integration-%d", os.Getpid())
	dockerEnvironment := append(os.Environ(), "DOCKER_HOST="+EndpointURL(socketPath), "DOCKER_CONTEXT=")
	compose := func(args ...string) *exec.Cmd {
		command := exec.CommandContext(ctx, "docker", append([]string{
			"compose", "--project-name", projectName, "--file", composeFile,
		}, args...)...)
		command.Env = dockerEnvironment
		return command
	}
	t.Cleanup(func() {
		cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		command := exec.CommandContext(cleanupContext, "docker",
			"compose", "--project-name", projectName, "--file", composeFile,
			"down", "--volumes", "--remove-orphans",
		)
		command.Env = dockerEnvironment
		_ = command.Run()
	})

	if output, err := compose("up", "--detach", "--no-build", "--pull", "never").CombinedOutput(); err != nil {
		t.Fatalf("docker compose up: %v: %s", err, output)
	}
	output, err := compose("exec", "-T", "client", "getent", "hosts", "server").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve Compose service hostname: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "server") {
		t.Fatalf("Compose service hostname was not resolved: %s", output)
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
