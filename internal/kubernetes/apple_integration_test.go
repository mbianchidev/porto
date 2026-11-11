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
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestAppleClusterIntegration(t *testing.T) {
	if os.Getenv("PORTO_APPLE_CLUSTER_INTEGRATION") != "1" {
		t.Skip("set PORTO_APPLE_CLUSTER_INTEGRATION=1 on Apple silicon with macOS 26+ and stable container 1.7.1+")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Fatal("Apple Cluster acceptance requires a native Apple silicon Mac")
	}
	for _, binary := range []string{"container", "kubectl"} {
		if _, err := runtimes.LookPath(binary); err != nil {
			t.Fatalf("required native acceptance tool %s: %v", binary, err)
		}
	}
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	externalConfig := filepath.Join(root, "unrelated-config")
	const unrelatedConfig = "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"
	if err := os.WriteFile(externalConfig, []byte(unrelatedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", externalConfig)
	ctx, cancel := context.WithTimeout(context.Background(), kubernetes.ClusterCreationTimeout+15*time.Minute)
	defer cancel()
	runner := runtimes.ExecRunner{}
	kubeRoot := filepath.Join(root, "kubernetes")
	provisioner := kubernetes.NewClusterProvisioner(nil, runner, kubeRoot)
	name := fmt.Sprintf("apple-it-%d", time.Now().UnixNano())
	currentName := name
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 8*time.Minute)
		defer stop()
		if err := provisioner.Delete(cleanup, currentName); err != nil {
			t.Errorf("remove exact native acceptance cluster %s: %v", currentName, err)
		}
	})
	t.Logf("creating native Apple cluster %s with one worker", name)
	cluster, err := provisioner.Create(ctx, kubernetes.ClusterRequest{
		Name: name, Provider: "apple",
		ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 3072},
		NodeGroups: []kubernetes.NodeGroupSpec{{
			Name: "workers", Count: 1, Machine: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048},
			Labels: map[string]string{"porto.test/fixture": "apple"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := kubernetes.NewWithKubeconfigRoot(runner, kubeRoot)
	if status := manager.Status(ctx, cluster.Context); !status.Available {
		t.Fatalf("native Kubernetes API is unavailable: %+v", status)
	}
	nodes, err := manager.Nodes(ctx, cluster.Context)
	if err != nil || len(nodes) != 2 {
		t.Fatalf("native nodes = %+v, error = %v", nodes, err)
	}
	run := func(action string, command runtimes.Command) []byte {
		t.Helper()
		commandContext, stop := context.WithTimeout(ctx, 6*time.Minute)
		defer stop()
		output, err := runner.Run(commandContext, command)
		if err != nil {
			t.Fatal(runtimes.CommandError(action, output, err))
		}
		return output
	}
	output := run("inspect native VM resource budgets", runtimes.Command{
		Name: "container", Args: append([]string{"inspect"}, cluster.Nodes...), StdoutOnly: true,
	})
	var nativeNodes []struct {
		Configuration struct {
			ID        string `json:"id"`
			Resources struct {
				CPUs          int   `json:"cpus"`
				MemoryInBytes int64 `json:"memoryInBytes"`
			} `json:"resources"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(output, &nativeNodes); err != nil {
		t.Fatal(err)
	}
	if len(nativeNodes) != 2 {
		t.Fatalf("native VM count = %d, want 2", len(nativeNodes))
	}
	for _, node := range nativeNodes {
		memory := int64(2048 * 1024 * 1024)
		if node.Configuration.ID == cluster.Nodes[0] {
			memory = 3072 * 1024 * 1024
		} else if node.Configuration.ID != cluster.Nodes[1] {
			t.Fatalf("unexpected native VM identity %s", node.Configuration.ID)
		}
		if node.Configuration.Resources.CPUs != 2 || node.Configuration.Resources.MemoryInBytes != memory {
			t.Fatalf("native resource budget for %s = %+v, want 2 CPUs and %d bytes", node.Configuration.ID, node.Configuration.Resources, memory)
		}
	}
	const image = "docker.io/library/busybox:1.37.0"
	t.Log("loading a native image into every node without sharing the host image store")
	run("pull synthetic workload image", runtimes.Command{
		Name: "container", Args: []string{"image", "pull", "--platform", "linux/arm64", image}, Network: true,
	})
	if err := provisioner.ImportImage(ctx, name, image); err != nil {
		t.Fatal(err)
	}
	kubectl := func(action string, input []byte, args ...string) []byte {
		t.Helper()
		arguments := append([]string{"--kubeconfig", cluster.KubeconfigPath, "--context", cluster.Context}, args...)
		return run(action, runtimes.Command{Name: "kubectl", Args: arguments, Stdin: input})
	}
	const fixture = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: apple-fixture-data
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: apple-fixture
  labels:
    app: apple-fixture
spec:
  nodeSelector:
    porto.test/fixture: apple
  containers:
    - name: app
      image: docker.io/library/busybox:1.37.0
      imagePullPolicy: Never
      command: [sh, -c, "printf synthetic-apple-persistence > /data/marker; exec httpd -f -p 8080 -h /data"]
      readinessProbe:
        httpGet:
          path: /marker
          port: 8080
        periodSeconds: 1
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: apple-fixture-data
---
apiVersion: v1
kind: Service
metadata:
  name: apple-fixture
spec:
  selector:
    app: apple-fixture
  ports:
    - name: http
      port: 8080
      targetPort: 8080
`
	kubectl("create synthetic workload and storage", []byte(fixture), "apply", "-f", "-")
	kubectl("wait for native cached-image workload", nil, "wait", "--for=condition=Ready", "pod/apple-fixture", "--timeout=5m")
	output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture", "app",
		[]string{"wget", "-qO-", "http://apple-fixture:8080/marker"}, nil)
	if err != nil || string(output) != "synthetic-apple-persistence" {
		t.Fatalf("native service request = %q, error = %v", output, err)
	}
	services, err := manager.Services(ctx, cluster.Context, "default")
	foundService := false
	for _, service := range services {
		foundService = foundService || service.Name == "apple-fixture"
	}
	if err != nil || !foundService {
		t.Fatalf("native service inventory = %+v, error = %v", services, err)
	}
	hostname := "apple-fixture-" + name
	if err := manager.ApplyHTTPRoute(ctx, cluster.Context, "default", "apple-fixture", 8080, hostname); err != nil {
		t.Fatal(err)
	}
	gatewayService, err := manager.GatewayServiceName(ctx, cluster.Context)
	if err != nil {
		t.Fatal(err)
	}
	routeContext, stopRoute := context.WithTimeout(ctx, 2*time.Minute)
	defer stopRoute()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err = manager.Exec(routeContext, cluster.Context, "default", "apple-fixture", "app",
			[]string{"wget", "-qO-", "--header", "Host: " + hostname + "." + config.LocalhostDomain,
				"http://" + gatewayService + ".envoy-gateway-system:80/marker"}, nil)
		if err == nil && string(output) == "synthetic-apple-persistence" {
			break
		}
		select {
		case <-routeContext.Done():
			t.Fatalf("native Gateway route did not serve the workload: %q, error = %v", output, err)
		case <-ticker.C:
		}
	}
	for _, action := range []string{"start", "stop", "scale", "upgrade", "upgrade-recover"} {
		if err := provisioner.CheckProviderOperation(name, action); err == nil || !strings.Contains(err.Error(), "Apple Cluster does not support") {
			t.Fatalf("unsupported native action %s returned %v", action, err)
		}
	}
	renamed := name + "-renamed"
	if err := provisioner.Rename(ctx, name, renamed); err != nil {
		t.Fatal(err)
	}
	currentName = renamed
	clusters, err := provisioner.List(ctx)
	if err != nil || len(clusters) != 1 || clusters[0].Name != renamed || clusters[0].State != "running" {
		t.Fatalf("renamed native inventory = %+v, error = %v", clusters, err)
	}
	cluster = clusters[0]
	output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture", "app", []string{"cat", "/data/marker"}, nil)
	if err != nil || string(output) != "synthetic-apple-persistence" {
		t.Fatalf("renaming lost workload data or selected identity: %q, error = %v", output, err)
	}
	if err := provisioner.Delete(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	clusters, err = provisioner.List(ctx)
	if err != nil || len(clusters) != 0 {
		t.Fatalf("deleted native cluster remains: %+v, error = %v", clusters, err)
	}
	data, err := os.ReadFile(externalConfig)
	if err != nil || string(data) != unrelatedConfig {
		t.Fatalf("native lifecycle changed unrelated kubeconfig: %v", err)
	}
	if _, err := os.Stat(cluster.KubeconfigPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted native kubeconfig remains: %v", err)
	}
}
