package kubernetes_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type upgradeIntegrationRunner struct {
	runtimes.ExecRunner
	dockerHost string
}

func (r upgradeIntegrationRunner) scoped(command runtimes.Command) runtimes.Command {
	if command.Name == "docker" || command.Name == "kind" {
		command.Env = append(command.Env, "DOCKER_HOST="+r.dockerHost, "DOCKER_CONTEXT=")
	}
	return command
}

func (r upgradeIntegrationRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	return r.ExecRunner.Run(ctx, r.scoped(command))
}

func (r upgradeIntegrationRunner) RunStreaming(ctx context.Context, command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
	return r.ExecRunner.RunStreaming(ctx, r.scoped(command), emit)
}

func TestKindUpgradeVolumeIntegration(t *testing.T) {
	if os.Getenv("PORTO_KUBERNETES_UPGRADE_INTEGRATION") != "1" {
		t.Skip("set PORTO_KUBERNETES_UPGRADE_INTEGRATION=1 and PORTO_KUBERNETES_TEST_DOCKER_HOST to a disposable-capable Porto Docker endpoint")
	}
	host := os.Getenv("PORTO_KUBERNETES_TEST_DOCKER_HOST")
	sourceEngine := os.Getenv("PORTO_KUBERNETES_UPGRADE_SOURCE_ENGINE") == "1"
	if !sourceEngine && !strings.HasPrefix(host, "unix://") && !strings.HasPrefix(host, "npipe://") {
		t.Fatal("explicit local Porto Docker endpoint is required")
	}
	temporaryRoot := os.TempDir()
	if runtime.GOOS == "darwin" {
		temporaryRoot = "/tmp"
	}
	root, err := os.MkdirTemp(temporaryRoot, "porto-upgrade-smoke-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove owned smoke state: %v", err)
		}
	})
	t.Setenv("PORTO_HOME", root)
	t.Setenv("DOCKER_CONFIG", filepath.Join(root, "private-docker-client"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	t.Cleanup(cancel)
	if sourceEngine {
		t.Setenv("LIMA_HOME", filepath.Join(root, "lima"))
		engineDirectory, err := config.DockerEngineDir()
		if err != nil {
			t.Fatal(err)
		}
		manager := portodocker.NewWithStateDir(nil, engineDirectory)
		t.Log("starting invocation-private Porto engine with the current source Docker API")
		if _, err := manager.InstallEngine(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
			defer stop()
			if err := manager.RemoveEngine(cleanup); err != nil {
				t.Errorf("remove invocation-private engine: %v", err)
			}
		})
		socketPath, err := config.DockerSocketPath()
		if err != nil {
			t.Fatal(err)
		}
		server := portodocker.NewAPIServer(socketPath, portodocker.NewAPI(manager, socketPath))
		if err := server.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := errors.Join(server.Close(cleanup), manager.StopContainerInventory(cleanup)); err != nil {
				t.Errorf("close private Docker API: %v", err)
			}
		})
		host = portodocker.EndpointURL(socketPath)
	}
	runner := upgradeIntegrationRunner{dockerHost: host}
	kubeRoot := filepath.Join(root, "kubernetes")
	p := kubernetes.NewClusterProvisioner(nil, runner, kubeRoot)
	p.SetRegistryConfigProvider(func(context.Context) ([]byte, error) { return []byte(`{"auths":{}}`), nil })
	name := fmt.Sprintf("upgrade-smoke-%d", time.Now().Unix())
	verifyUpgradeDockerStdin(t, ctx, runner, name+"-stdin")
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Minute)
		defer stop()
		var spec kubernetes.ClusterRequest
		data, err := os.ReadFile(filepath.Join(kubeRoot, config.KubernetesClusterFileToken(name)+".json"))
		if err == nil {
			err = json.Unmarshal(data, &spec)
		}
		if err != nil {
			t.Errorf("read owned smoke cluster for cleanup: %v", err)
			return
		}
		owned := map[string]bool{upgradeRuntimeName(spec): true, name: true}
		if spec.Upgrade != nil && spec.Upgrade.BackupPath != "" {
			data, err := os.ReadFile(filepath.Join(spec.Upgrade.BackupPath, "recovery.json"))
			if err == nil {
				var backup struct {
					Original    kubernetes.ClusterRequest `json:"original"`
					Replacement kubernetes.ClusterRequest `json:"replacement"`
				}
				if err := json.Unmarshal(data, &backup); err == nil && backup.Original.Name == name && backup.Replacement.Name == name {
					owned[upgradeRuntimeName(backup.Original)] = true
					owned[upgradeRuntimeName(backup.Replacement)] = true
				}
			}
		}
		for runtimeName := range owned {
			output, err := runner.Run(cleanup, runtimes.Command{Name: "kind", Args: []string{"delete", "cluster", "--name", "porto-" + runtimeName}})
			if err != nil {
				t.Errorf("clean up exact smoke runtime %s: %v: %s", runtimeName, err, output)
			}
		}
	})
	t.Logf("creating isolated smoke cluster %s", name)
	if _, err := p.Create(ctx, kubernetes.ClusterRequest{
		Name: name, Provider: "kind", Version: "v1.36.4",
		ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
		NodeGroups:   []kubernetes.NodeGroupSpec{{Name: "workers", Count: 1, Machine: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}}},
	}); err != nil {
		t.Fatal(err)
	}
	run := func(action string, input []byte, args ...string) ([]byte, error) {
		commandCtx, stop := context.WithTimeout(ctx, 6*time.Minute)
		defer stop()
		arguments := append([]string{"--kubeconfig", filepath.Join(kubeRoot, config.KubernetesClusterFileToken(name)+".yaml"), "--context", "porto-" + name}, args...)
		output, err := runner.Run(commandCtx, runtimes.Command{Name: "kubectl", Args: arguments, Stdin: input})
		if err != nil {
			return output, fmt.Errorf("%s: %w: %s", action, err, output)
		}
		return output, nil
	}
	const fixture = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: fixture-data
  namespace: default
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fixture-app
  namespace: default
spec:
  replicas: 1
  selector:
    matchLabels:
      app: upgrade-fixture
  template:
    metadata:
      labels:
        app: upgrade-fixture
    spec:
      containers:
        - name: fixture
          image: busybox:1.37.0
          command: [sleep, "36000"]
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: fixture-data
`
	if _, err := run("create synthetic persistent application", []byte(fixture), "apply", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("wait for source application", nil, "rollout", "status", "deployment/fixture-app", "--timeout=5m"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("write source-only synthetic marker", nil, "exec", "deployment/fixture-app", "--", "sh", "-c", "printf synthetic-persistent-data > /data/marker"); err != nil {
		t.Fatal(err)
	}
	readMarker := func() {
		t.Helper()
		output, err := run("read synthetic persistent marker", nil, "exec", "deployment/fixture-app", "--", "cat", "/data/marker")
		if err != nil || string(output) != "synthetic-persistent-data" {
			t.Fatalf("persistent marker = %q, error %v", output, err)
		}
	}
	readMarker()
	t.Log("upgrading isolated kind cluster and verifying persistent data")
	if _, err := p.UpgradeCluster(ctx, name, kubernetes.UpgradeRequest{Version: "v1.37.0", Confirm: true}); err != nil {
		t.Fatal(err)
	}
	readMarker()
	t.Log("kind replacement retained the synthetic volume and restarted its application")
}

func upgradeRuntimeName(spec kubernetes.ClusterRequest) string {
	if spec.RuntimeName != "" {
		return spec.RuntimeName
	}
	return spec.Name
}

func verifyUpgradeDockerStdin(t *testing.T, ctx context.Context, runner upgradeIntegrationRunner, name string) {
	t.Helper()
	output, err := runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"run", "-d", "--name", name, "busybox:1.37.0", "sleep", "3600"}})
	if err != nil {
		t.Fatalf("create scoped stdin probe: %v: %s", err, output)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if output, err := runner.Run(cleanup, runtimes.Command{Name: "docker", Args: []string{"rm", "-f", name}}); err != nil {
			t.Errorf("remove exact stdin probe %s: %v: %s", name, err, output)
		}
	})
	probe, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	input := []byte("synthetic-docker-stdin\n")
	output, err = runner.Run(probe, runtimes.Command{Name: "docker", Args: []string{"exec", "-i", name, "cat"}, Stdin: input})
	if err != nil || string(output) != string(input) {
		t.Fatalf("Docker stdin preflight failed before kind creation: %v: %q", err, output)
	}
}
