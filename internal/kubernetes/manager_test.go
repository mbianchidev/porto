package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

type fakeRunner struct {
	mu        sync.Mutex
	commands  []runtimes.Command
	instances map[string]bool
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{instances: map[string]bool{}}
}

func (f *fakeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	joined := strings.Join(command.Args, " ")
	switch command.Name {
	case "kubectl":
		switch {
		case strings.Contains(joined, "version -o json"):
			return []byte(`{"clientVersion":{"gitVersion":"v1.33.0"},"serverVersion":{"gitVersion":"v1.33.1"}}`), nil
		case strings.Contains(joined, "config current-context"):
			return []byte("porto-dev\n"), nil
		case strings.Contains(joined, "config view -o json"):
			return []byte(`{"current-context":"porto-dev","contexts":[{"name":"porto-dev","context":{"cluster":"porto-dev","user":"porto-dev","namespace":"default"}}]}`), nil
		case strings.Contains(joined, "get pods"):
			return []byte(`{"items":[{"metadata":{"name":"api-1","namespace":"dev","creationTimestamp":"2026-08-30T20:00:00Z"},"spec":{"nodeName":"worker-1","containers":[{"name":"api","image":"porto/api:latest"}]},"status":{"phase":"Running","podIP":"10.42.0.2","containerStatuses":[{"name":"api","ready":true,"restartCount":1,"state":{"running":{}}}]}}]}`), nil
		case strings.Contains(joined, "get services"):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(joined, "get nodes"):
			return []byte(`{"items":[]}`), nil
		case strings.Contains(joined, "config rename-context"):
			return []byte("Context renamed"), nil
		}
	case "limactl":
		if len(command.Args) > 0 {
			switch command.Args[0] {
			case "create":
				for index, arg := range command.Args {
					if arg == "--name" && index+1 < len(command.Args) {
						f.instances[command.Args[index+1]] = true
					}
				}
				return nil, nil
			case "start", "stop":
				return nil, nil
			case "delete":
				name := command.Args[len(command.Args)-1]
				delete(f.instances, name)
				return nil, nil
			case "list":
				var output strings.Builder
				for name := range f.instances {
					output.WriteString(`{"name":"` + name + `","status":"Running","arch":"aarch64","cpus":2,"memory":2147483648,"disk":21474836480}`)
					output.WriteByte('\n')
				}
				return []byte(output.String()), nil
			case "shell":
				switch {
				case strings.Contains(joined, "hostname -I"):
					return []byte("192.168.105.2\n"), nil
				case strings.Contains(joined, "server/node-token"):
					return []byte("test-token\n"), nil
				case strings.Contains(joined, "k3s.yaml"):
					return []byte("apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\ncurrent-context: default\n"), nil
				default:
					return nil, nil
				}
			}
		}
	}
	return nil, nil
}

func TestStatusAndPodDecoding(t *testing.T) {
	manager := New(newFakeRunner())
	status := manager.Status(context.Background(), "")
	if !status.Available || status.Context != "porto-dev" || status.ServerVersion != "v1.33.1" {
		t.Fatalf("unexpected status: %+v", status)
	}
	pods, err := manager.Pods(context.Background(), "porto-dev", "all")
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods) != 1 || pods[0].Ready != "1/1" || pods[0].Restarts != 1 || pods[0].Containers[0].State != "running" {
		t.Fatalf("unexpected pods: %+v", pods)
	}
	contexts, err := manager.Contexts(context.Background())
	if err != nil {
		t.Fatalf("list contexts: %v", err)
	}
	if len(contexts) != 1 || contexts[0].Cluster != "porto-dev" || !contexts[0].Current {
		t.Fatalf("unexpected contexts: %+v", contexts)
	}
}

func TestManagedContextUsesPrivateKubeconfig(t *testing.T) {
	dir := t.TempDir()
	kubeconfigPath := filepath.Join(dir, "dev.yaml")
	if err := os.WriteFile(kubeconfigPath, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := newFakeRunner()
	manager := NewWithKubeconfigRoot(runner, dir)
	if _, err := manager.Pods(context.Background(), "porto-dev", "all"); err != nil {
		t.Fatalf("list managed pods: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	args := runner.commands[len(runner.commands)-1].Args
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--kubeconfig "+kubeconfigPath+" --context porto-dev") {
		t.Fatalf("managed context did not use private kubeconfig: %v", args)
	}
}

func TestFileOperationsKeepPathAsArgument(t *testing.T) {
	runner := newFakeRunner()
	manager := New(runner)
	if err := manager.WriteFile(
		context.Background(),
		"porto-dev",
		"dev",
		"api-1",
		"api",
		"/app/config.json",
		[]byte(`{"ok":true}`),
	); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 {
		t.Fatalf("unexpected commands: %+v", runner.commands)
	}
	command := runner.commands[0]
	if command.Args[len(command.Args)-1] != "/app/config.json" {
		t.Fatalf("path was not passed as a separate argument: %+v", command.Args)
	}
	if string(command.Stdin) != `{"ok":true}` {
		t.Fatalf("unexpected stdin: %q", command.Stdin)
	}
}

func TestProvisionClusterCreatesVMBackedNodesAndKubeconfig(t *testing.T) {
	runner := newFakeRunner()
	vmManager := vm.New(runner)
	dir := t.TempDir()
	provisioner := NewClusterProvisioner(vmManager, runner, dir)
	cluster, err := provisioner.Create(context.Background(), ClusterRequest{
		Name:         "dev",
		ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 2, Machine: MachineSpec{CPUs: 4, MemoryMiB: 4096, DiskGiB: 30},
		}},
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	if len(cluster.Nodes) != 3 || cluster.Context != "porto-dev" {
		t.Fatalf("unexpected cluster: %+v", cluster)
	}
	data, err := os.ReadFile(filepath.Join(dir, "dev.yaml"))
	if err != nil {
		t.Fatalf("read kubeconfig: %v", err)
	}
	if !strings.Contains(string(data), "https://127.0.0.1:") || strings.Contains(string(data), "https://127.0.0.1:6443") {
		t.Fatalf("kubeconfig did not use the forwarded host API port: %s", data)
	}
	if strings.Contains(string(data), "name: default") || !strings.Contains(string(data), "name: porto-dev") {
		t.Fatalf("kubeconfig identifiers were not made cluster-specific: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev.json")); err != nil {
		t.Fatalf("cluster metadata missing: %v", err)
	}
}

func TestImportImageCopiesArchiveToEveryClusterNode(t *testing.T) {
	runner := newFakeRunner()
	runner.instances["porto-dev-server-1"] = true
	runner.instances["porto-dev-workers-1"] = true
	runner.instances["unrelated"] = true
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{
		Name: "dev",
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 1,
		}},
	}); err != nil {
		t.Fatalf("write cluster metadata: %v", err)
	}
	if err := provisioner.ImportImage(context.Background(), "dev", "example:dev"); err != nil {
		t.Fatalf("import image: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	imports := 0
	for _, command := range runner.commands {
		if command.Name == "limactl" && strings.Contains(strings.Join(command.Args, " "), "k3s ctr images import") {
			imports++
		}
	}
	if imports != 2 {
		t.Fatalf("image imports = %d, want 2", imports)
	}
}

func TestClusterDeletionUsesExactMetadataOwnership(t *testing.T) {
	runner := newFakeRunner()
	runner.instances["porto-app-server-1"] = true
	runner.instances["porto-app-v2-server-1"] = true
	provisioner := NewClusterProvisioner(vm.New(runner), runner, t.TempDir())
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "app"}); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.writeClusterMetadata(ClusterRequest{Name: "app-v2"}); err != nil {
		t.Fatal(err)
	}
	if err := provisioner.Delete(context.Background(), "app"); err != nil {
		t.Fatalf("delete app cluster: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.instances["porto-app-server-1"] {
		t.Fatal("app server VM was not deleted")
	}
	if !runner.instances["porto-app-v2-server-1"] {
		t.Fatal("app-v2 server VM was deleted by prefix collision")
	}
}
