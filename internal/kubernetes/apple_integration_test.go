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
		t.Skip("set PORTO_APPLE_CLUSTER_INTEGRATION=1 on a disposable-capable native Apple silicon host")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Fatal("native Apple silicon execution is required")
	}
	control := newAppleIsolationControl(t)
	for _, workers := range []int{0, 1} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			exerciseAppleClusterIntegration(t, workers, control)
		})
	}
}

func exerciseAppleClusterIntegration(t *testing.T, workers int, control *appleIsolationControl) {
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
	root, err := os.MkdirTemp("", "porto-native-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", root)
	externalConfig := filepath.Join(root, "unrelated-config")
	const unrelatedConfig = "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"
	if err := os.WriteFile(externalConfig, []byte(unrelatedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", externalConfig)
	ctx, cancel := context.WithTimeout(context.Background(), kubernetes.ClusterCreationTimeout+15*time.Minute)
	defer cancel()
	runner := &appleAcceptanceRunner{}
	kubeRoot := filepath.Join(root, "kubernetes")
	provisioner := kubernetes.NewClusterProvisioner(nil, runner, kubeRoot)
	name := fmt.Sprintf("apple-it-%d", time.Now().UnixNano())
	currentName := name
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 8*time.Minute)
		defer stop()
		if err := provisioner.Delete(cleanup, currentName); err != nil {
			t.Errorf("remove exact native acceptance cluster %s: %v; ownership state retained at %s", currentName, err, root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove owned native fixture state: %v", err)
		}
	})
	t.Logf("creating native Apple cluster %s with %d workers", name, workers)
	request := kubernetes.ClusterRequest{
		Name: name, Provider: "apple",
		ControlPlane: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 3072},
	}
	if workers > 0 {
		request.NodeGroups = []kubernetes.NodeGroupSpec{{
			Name: "workers", Count: workers, Machine: kubernetes.MachineSpec{CPUs: 2, MemoryMiB: 2048},
			Labels: map[string]string{"porto.test/fixture": "apple"},
		}}
	}
	cluster, err := provisioner.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	manager := kubernetes.NewWithKubeconfigRoot(runner, kubeRoot)
	if status := manager.Status(ctx, cluster.Context); !status.Available {
		t.Fatalf("native Kubernetes API is unavailable: %+v", status)
	}
	nodes, err := manager.Nodes(ctx, cluster.Context)
	if err != nil || len(nodes) != workers+1 {
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
	if len(nativeNodes) != workers+1 {
		t.Fatalf("native VM count = %d, want %d", len(nativeNodes), workers+1)
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
	if workers == 0 {
		kubectl("label exact single-node fixture", nil, "label", "node", cluster.Nodes[0], "porto.test/fixture=apple", "--overwrite=true")
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
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: apple-fixture
spec:
  serviceName: apple-fixture
  replicas: 1
  selector:
    matchLabels:
      app: apple-fixture
  template:
    metadata:
      labels:
        app: apple-fixture
    spec:
      nodeSelector:
        porto.test/fixture: apple
      containers:
        - name: app
          image: docker.io/library/busybox:1.37.0
          imagePullPolicy: Never
          command: [sh, -c, "test -f /data/marker || printf synthetic-apple-persistence > /data/marker; exec httpd -f -p 8080 -h /data"]
          lifecycle:
            preStop:
              exec:
                command: [sh, -c, "printf graceful-shutdown >> /data/shutdowns; sync"]
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
---
apiVersion: batch/v1
kind: Job
metadata:
  name: already-completed
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: done
          image: docker.io/library/busybox:1.37.0
          imagePullPolicy: Never
          command: [sh, -c, "exit 0"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: intentionally-paused
spec:
  paused: true
  replicas: 1
  selector:
    matchLabels:
      app: intentionally-paused
  template:
    metadata:
      labels:
        app: intentionally-paused
    spec:
      containers:
        - name: paused
          image: docker.io/library/busybox:1.37.0
          imagePullPolicy: Never
          command: [sleep, "3600"]
`
	kubectl("create synthetic workload and storage", []byte(fixture), "apply", "-f", "-")
	kubectl("wait for native cached-image workload", nil, "rollout", "status", "statefulset/apple-fixture", "--timeout=5m")
	kubectl("wait for completed fixture job", nil, "wait", "--for=condition=Complete", "job/already-completed", "--timeout=5m")
	before, err := provisioner.Configuration(name)
	if err != nil {
		t.Fatal(err)
	}
	pvcUID := string(kubectl("record persistent claim identity", nil, "get", "pvc", "apple-fixture-data", "-o", "jsonpath={.metadata.uid}"))
	control.assertUnaffected(t, ctx)
	output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture-0", "app",
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
		output, err = manager.Exec(routeContext, cluster.Context, "default", "apple-fixture-0", "app",
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
	for _, action := range []string{"scale", "upgrade", "upgrade-recover"} {
		if err := provisioner.CheckProviderOperation(name, action); err == nil || !strings.Contains(err.Error(), "Apple Cluster does not support") {
			t.Fatalf("unsupported native action %s returned %v", action, err)
		}
	}
	for cycle := range 2 {
		t.Logf("gracefully stopping and recovering persistent native cluster, cycle %d", cycle+1)
		if err := provisioner.SetRunning(ctx, name, false); err != nil {
			t.Fatal(err)
		}
		control.assertUnaffected(t, ctx)
		if err := provisioner.SetRunning(ctx, name, false); err != nil {
			t.Fatalf("repeated Stop was not idempotent: %v", err)
		}
		if _, err := provisioner.Start(ctx, name); err != nil {
			t.Fatal(err)
		}
		control.assertUnaffected(t, ctx)
		if _, err := provisioner.Start(ctx, name); err != nil {
			t.Fatalf("repeated Start was not idempotent: %v", err)
		}
		output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture-0", "app",
			[]string{"wget", "-qO-", "http://apple-fixture:8080/marker"}, nil)
		if err != nil || string(output) != "synthetic-apple-persistence" {
			t.Fatalf("persistent Service did not recover: %q, error = %v", output, err)
		}
		output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture-0", "app",
			[]string{"cat", "/data/shutdowns"}, nil)
		if err != nil || strings.Count(string(output), "graceful-shutdown") < cycle+1 {
			t.Fatalf("kubelet did not run workload preStop before node VM shutdown: %q, error = %v", output, err)
		}
		after, err := provisioner.Configuration(name)
		if err != nil || after.Apple.CAHash != before.Apple.CAHash {
			t.Fatalf("recovery changed the cluster TLS authority: %v", err)
		}
		changedAddress := false
		for index, node := range after.Apple.Nodes {
			previous := before.Apple.Nodes[index]
			if node.ID != previous.ID || node.CreatedAt != previous.CreatedAt || node.Storage.RootfsID != previous.Storage.RootfsID ||
				node.KubernetesUID != previous.KubernetesUID || node.PodCIDR != previous.PodCIDR {
				t.Fatalf("recovery replaced node identity, membership or backing data: %s", node.ID)
			}
			changedAddress = changedAddress || node.IPv4 != previous.IPv4
		}
		if !changedAddress {
			t.Fatal("address-change recovery was not exercised")
		}
		before = after
		if string(kubectl("verify persistent claim identity", nil, "get", "pvc", "apple-fixture-data", "-o", "jsonpath={.metadata.uid}")) != pvcUID ||
			string(kubectl("verify desired replicas", nil, "get", "statefulset", "apple-fixture", "-o", "jsonpath={.spec.replicas}")) != "1" {
			t.Fatal("native lifecycle changed durable storage identity or desired replicas")
		}
	}
	if workers > 0 {
		if err := provisioner.SetRunning(ctx, name, false); err != nil {
			t.Fatal(err)
		}
		runner.failNextStart(cluster.Nodes[1])
		if _, err := provisioner.Start(ctx, name); err == nil || !strings.Contains(err.Error(), "synthetic native worker start failure") {
			t.Fatalf("partial start failure was hidden: %v", err)
		}
		states := appleContainerCheckpoints(t, ctx, runner, cluster.Nodes)
		if states[cluster.Nodes[0]].State != "running" || states[cluster.Nodes[1]].State != "stopped" {
			t.Fatalf("partial start did not retain dependency order: %v", states)
		}
		control.assertUnaffected(t, ctx)
		controlStart := states[cluster.Nodes[0]].Started
		provisioner = kubernetes.NewClusterProvisioner(nil, runner, kubeRoot)
		if _, err := provisioner.Start(ctx, name); err != nil {
			t.Fatalf("new application instance could not resume persisted progress: %v", err)
		}
		states = appleContainerCheckpoints(t, ctx, runner, cluster.Nodes)
		if states[cluster.Nodes[0]].Started != controlStart {
			t.Fatal("retry restarted an already recovered control-plane VM")
		}
		control.assertUnaffected(t, ctx)
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
	output, err = manager.Exec(ctx, cluster.Context, "default", "apple-fixture-0", "app", []string{"cat", "/data/marker"}, nil)
	if err != nil || string(output) != "synthetic-apple-persistence" {
		t.Fatalf("renaming lost workload data or selected identity: %q, error = %v", output, err)
	}
	if err := provisioner.Delete(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	control.assertUnaffected(t, ctx)
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
