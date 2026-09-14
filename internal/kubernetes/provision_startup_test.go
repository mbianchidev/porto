package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
	"gopkg.in/yaml.v3"
)

type startupNodeConfig struct {
	Base     string `yaml:"base"`
	VMType   string `yaml:"vmType"`
	CPUs     int    `yaml:"cpus"`
	Memory   string `yaml:"memory"`
	Disk     string `yaml:"disk"`
	Networks []struct {
		Lima string `yaml:"lima"`
	} `yaml:"networks"`
	PortForwards []struct {
		GuestPort int    `yaml:"guestPort"`
		HostPort  int    `yaml:"hostPort"`
		Proto     string `yaml:"proto"`
		Static    bool   `yaml:"static"`
	} `yaml:"portForwards"`
}

type clusterStartupRunner struct {
	mu           sync.Mutex
	delegate     *fakeRunner
	commands     []runtimes.Command
	configs      map[string]startupNodeConfig
	configPaths  []string
	startBudgets []time.Duration
	fault        func(context.Context, runtimes.Command) ([]byte, error, bool)
}

func newClusterStartupRunner() *clusterStartupRunner {
	return &clusterStartupRunner{delegate: newFakeRunner(), configs: make(map[string]startupNodeConfig)}
}

func (r *clusterStartupRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	if command.Name == "limactl" {
		switch command.Args[0] {
		case "create":
			if !slices.Contains(command.Args, "--mount-none") || !slices.Contains(command.Args, "--containerd=none") {
				return []byte("Kubernetes nodes must not require Windows reverse-sshfs or Lima containerd"), errors.New("unexpected node profile")
			}
			nameIndex := slices.Index(command.Args, "--name")
			if nameIndex < 0 || nameIndex+1 >= len(command.Args) {
				return nil, errors.New("node name missing")
			}
			configPath := command.Args[len(command.Args)-1]
			data, err := os.ReadFile(configPath)
			if err != nil {
				return nil, err
			}
			var config startupNodeConfig
			if err := yaml.Unmarshal(data, &config); err != nil {
				return nil, err
			}
			r.configs[command.Args[nameIndex+1]] = config
			r.configPaths = append(r.configPaths, configPath)
		case "start":
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("Lima start has no deadline")
			}
			r.startBudgets = append(r.startBudgets, time.Until(deadline))
		}
	}
	if r.fault != nil {
		if output, err, handled := r.fault(ctx, command); handled {
			return output, err
		}
	}
	if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), "config view --raw -o json") {
		data, err := os.ReadFile(command.Args[1])
		if err != nil {
			return nil, err
		}
		var document map[string]any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return nil, err
		}
		return json.Marshal(document)
	}
	return r.delegate.Run(ctx, command)
}

func TestVMClusterLifecycleAvoidsWindowsSSHFSDependencies(t *testing.T) {
	for _, provider := range []string{"k3s", "k0s"} {
		t.Run(provider, func(t *testing.T) {
			runner := newClusterStartupRunner()
			runner.delegate.instances["porto-synthetic-cluster-neighbor-server-1"] = true
			vmState := t.TempDir()
			vmManager := vm.NewWithStateDir(runner, vmState)
			provisioner := NewClusterProvisioner(vmManager, runner, t.TempDir())
			request := ClusterRequest{
				Name: "synthetic-cluster", Provider: provider, APIPort: 58443,
				ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
				NodeGroups: []NodeGroupSpec{{
					Name: "workers", Count: 1, Machine: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
				}},
			}
			wantNodes := []string{"porto-synthetic-cluster-server-1", "porto-synthetic-cluster-workers-1"}

			cluster, err := provisioner.Create(context.Background(), request)
			if err != nil {
				t.Fatalf("create cluster: %v", err)
			}
			if cluster.Provider != provider || cluster.State != "running" || !slices.Equal(cluster.Nodes, wantNodes) {
				t.Fatalf("unexpected cluster: %+v", cluster)
			}
			if cluster.Server != "https://127.0.0.1:58443" {
				t.Fatalf("API endpoint = %q", cluster.Server)
			}
			for index, name := range wantNodes {
				config, ok := runner.configs[name]
				if !ok {
					t.Fatalf("missing generated config for %s", name)
				}
				if config.Base != "template:ubuntu-24.04" || config.VMType != "" {
					t.Errorf("node image/host-default driver changed (Windows must remain QEMU, not WSL2): %+v", config)
				}
				if config.CPUs != 2 || config.Memory != "2048MiB" || config.Disk != "20GiB" {
					t.Errorf("node resources were lost: %+v", config)
				}
				if len(config.Networks) != 1 || config.Networks[0].Lima != "user-v2" {
					t.Errorf("inter-node networking changed: %+v", config.Networks)
				}
				if index == 0 {
					if len(config.PortForwards) != 1 {
						t.Fatalf("control-plane API forward = %+v", config.PortForwards)
					}
					forward := config.PortForwards[0]
					if forward.GuestPort != 6443 || forward.HostPort != request.APIPort || forward.Proto != "tcp" || !forward.Static {
						t.Errorf("API forward = %+v", forward)
					}
				} else if len(config.PortForwards) != 0 {
					t.Errorf("worker has unexpected API forwards: %+v", config.PortForwards)
				}
			}
			kubeconfig, err := os.ReadFile(cluster.KubeconfigPath)
			if err != nil || !strings.Contains(string(kubeconfig), cluster.Server) {
				t.Fatalf("configured API endpoint missing from kubeconfig: %s; error: %v", kubeconfig, err)
			}
			assertVMDistributionInstalled(t, runner.commands, provider, wantNodes)

			if err := provisioner.SetRunning(context.Background(), request.Name, false); err != nil {
				t.Fatalf("stop cluster: %v", err)
			}
			if recreated, err := provisioner.Start(context.Background(), request.Name); err != nil || recreated {
				t.Fatalf("restart cluster: recreated=%t err=%v", recreated, err)
			}
			if err := provisioner.Delete(context.Background(), request.Name); err != nil {
				t.Fatalf("delete cluster: %v", err)
			}
			var lifecycle []string
			for _, command := range runner.commands {
				if command.Name == "limactl" && slices.Contains([]string{"start", "stop", "delete"}, command.Args[0]) {
					lifecycle = append(lifecycle, strings.Join(command.Args, " "))
				}
			}
			wantLifecycle := []string{
				"start " + wantNodes[0], "start " + wantNodes[1],
				"stop " + wantNodes[1], "stop " + wantNodes[0],
				"start " + wantNodes[0], "start " + wantNodes[1],
				"delete --force " + wantNodes[1], "delete --force " + wantNodes[0],
			}
			if !slices.Equal(lifecycle, wantLifecycle) {
				t.Errorf("VM lifecycle = %v, want %v", lifecycle, wantLifecycle)
			}
			if len(runner.startBudgets) != 4 {
				t.Fatalf("start budgets = %v", runner.startBudgets)
			}
			for index, want := range []time.Duration{20 * time.Minute, 20 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
				if got := runner.startBudgets[index]; got < want-time.Second || got > want {
					t.Errorf("start %d budget = %s, want %s", index, got, want)
				}
			}
			if !reflect.DeepEqual(runner.delegate.instances, map[string]bool{"porto-synthetic-cluster-neighbor-server-1": true}) {
				t.Errorf("foreign instance changed or owned VMs survived: %v", runner.delegate.instances)
			}
			owned, err := vmManager.KubernetesNodeNames(request.Name)
			if err != nil || len(owned) != 0 {
				t.Errorf("node metadata was not cleaned up: %v, %v", owned, err)
			}
			for _, path := range append(runner.configPaths, cluster.KubeconfigPath, provisioner.clusterMetadataPath(request.Name)) {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("cluster/configuration file survived cleanup: %s, %v", path, err)
				}
			}
		})
	}
}

func assertVMDistributionInstalled(t *testing.T, commands []runtimes.Command, provider string, nodes []string) {
	t.Helper()
	for index, name := range nodes {
		var shellCommands []string
		for _, command := range commands {
			if command.Name == "limactl" && len(command.Args) > 1 && command.Args[0] == "shell" && command.Args[1] == name {
				shellCommands = append(shellCommands, strings.Join(command.Args[3:], " "))
			}
		}
		joined := strings.Join(shellCommands, "\n")
		var required []string
		if provider == "k3s" {
			required = []string{"curl -sfL https://get.k3s.io", "sh /tmp/porto-install-k3s.sh"}
			if index == 0 {
				required = append(required, "server --disable traefik --node-name "+name+" --write-kubeconfig-mode 600")
			} else {
				required = append(required, "K3S_URL=https://192.168.105.2:6443", "K3S_TOKEN=test-token", "agent --node-name "+name)
			}
		} else {
			required = []string{"curl -sSLf https://get.k0s.sh", "sh /tmp/porto-install-k0s.sh", "sudo k0s start", "sudo k0s status"}
			if index == 0 {
				required = append(required, "k0s install controller --enable-worker --no-taints")
			} else {
				required = append(required, "k0s install worker --token-file")
				foundToken := false
				for _, command := range commands {
					if len(command.Args) > 1 && command.Args[1] == name && string(command.Stdin) == "test-k0s-token" {
						foundToken = true
					}
				}
				if !foundToken {
					t.Errorf("%s did not receive its synthetic worker token", name)
				}
			}
		}
		for _, expected := range required {
			if !strings.Contains(joined, expected) {
				t.Errorf("%s provisioning missing %q", name, expected)
			}
		}
		if strings.Contains(joined, "INSTALL_K3S_VERSION=") || strings.Contains(joined, "K0S_VERSION=") {
			t.Errorf("%s did not preserve the default distribution version", name)
		}
	}
}

func TestVMClusterFailureCleansOnlyProvenCreatedNodes(t *testing.T) {
	for _, provider := range []string{"k3s", "k0s"} {
		for _, stage := range []string{"create-server", "start-server", "start-worker", "provision-worker"} {
			t.Run(provider+"/"+stage, func(t *testing.T) {
				runner := newClusterStartupRunner()
				const server = "porto-synthetic-failure-server-1"
				const worker = "porto-synthetic-failure-workers-1"
				const foreign = "porto-synthetic-failure-neighbor-server-1"
				const diagnostic = "synthetic VM startup failure"
				runner.delegate.instances[foreign] = true
				runner.fault = func(_ context.Context, command runtimes.Command) ([]byte, error, bool) {
					if command.Name != "limactl" {
						return nil, nil, false
					}
					args := command.Args
					joined := strings.Join(args, " ")
					fail := stage == "create-server" && args[0] == "create" && slices.Contains(args, server) ||
						stage == "start-server" && joined == "start "+server ||
						stage == "start-worker" && joined == "start "+worker ||
						stage == "provision-worker" && args[0] == "shell" && args[1] == worker &&
							(strings.Contains(joined, "agent --node-name") || strings.Contains(joined, "k0s install worker"))
					if fail {
						if strings.HasPrefix(stage, "start-") {
							return []byte(diagnostic), context.DeadlineExceeded, true
						}
						return []byte(diagnostic), errors.New("synthetic command failure"), true
					}
					return nil, nil, false
				}
				vmState := t.TempDir()
				vmManager := vm.NewWithStateDir(runner, vmState)
				provisioner := NewClusterProvisioner(vmManager, runner, t.TempDir())
				request := ClusterRequest{
					Name: "synthetic-failure", Provider: provider, APIPort: 58444,
					ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20},
					NodeGroups:   []NodeGroupSpec{{Name: "workers", Count: 1, Machine: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}}},
				}

				_, err := provisioner.Create(context.Background(), request)
				if err == nil || !strings.Contains(err.Error(), diagnostic) {
					t.Fatalf("failure lost its startup diagnostic: %v", err)
				}
				if strings.HasPrefix(stage, "start-") && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("startup failure lost the deadline cause: %v", err)
				}
				var deleted []string
				for _, command := range runner.commands {
					if command.Name == "limactl" && command.Args[0] == "delete" {
						deleted = append(deleted, command.Args[len(command.Args)-1])
					}
				}
				var wantDeleted []string
				if stage == "start-server" {
					wantDeleted = []string{server}
				} else if stage != "create-server" {
					wantDeleted = []string{worker, server}
				}
				if !slices.Equal(deleted, wantDeleted) {
					t.Errorf("cleanup = %v, want only proven creates %v", deleted, wantDeleted)
				}
				if !reflect.DeepEqual(runner.delegate.instances, map[string]bool{foreign: true}) {
					t.Errorf("foreign instance changed or owned nodes leaked: %v", runner.delegate.instances)
				}
				for _, name := range []string{server, worker} {
					if _, err := os.Stat(filepath.Join(vmState, name+".json")); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("stale VM ownership for %s: %v", name, err)
					}
				}
				failed, err := provisioner.readClusterMetadata(request.Name)
				if err != nil || failed.Phase != "error" || !strings.Contains(failed.Error, diagnostic) {
					t.Errorf("failure not persisted: %+v, %v", failed, err)
				}
				if _, err := os.Stat(provisioner.clusterKubeconfigPath(request.Name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("failed cluster left a kubeconfig: %v", err)
				}
				if err := provisioner.Delete(context.Background(), request.Name); err != nil {
					t.Errorf("delete failed cluster record: %v", err)
				}
			})
		}
	}
}
