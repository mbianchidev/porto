package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func autoscalingVersionFixture(t *testing.T, provider, metadataVersion, actualVersion string, versionError error) (*ClusterProvisioner, *fakeRunner, NodeGroupSpec) {
	t.Helper()
	root := filepath.Join(".autoscaling-version-test-state", strings.ReplaceAll(t.Name(), "/", "-"))
	vmRoot := filepath.Join(root, "vms")
	if err := os.MkdirAll(vmRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root); _ = os.Remove(".autoscaling-version-test-state") })
	group := NodeGroupSpec{Name: "as-0123456789", Count: 1, Machine: MachineSpec{CPUs: 2, MemoryMiB: 2048, DiskGiB: 20}}
	server, worker := "porto-stable-server-1", "porto-stable-"+group.Name+"-1"
	for _, name := range []string{server, worker} {
		data, err := json.Marshal(vm.Metadata{Name: name, Kind: "kubernetes-node", Owner: "dev"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vmRoot, name+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	runner := newFakeRunner()
	healthChecks := 0
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case command.Name == "limactl" && len(command.Args) != 0 && command.Args[0] == "list":
			return []byte(`{"name":"` + server + `","status":"Running"}` + "\n" + `{"name":"` + worker + `","status":"Running"}` + "\n"), nil
		case strings.Contains(joined, "sudo "+provider+" --version"):
			if !strings.Contains(joined, "shell "+server+" --") {
				t.Error("installed version was not read from the runtime's controller")
			}
			return []byte(provider + " version " + actualVersion + "\n"), versionError
		case strings.Contains(joined, "systemctl is-active k3s-agent") || strings.HasSuffix(joined, "sudo k0s status"):
			healthChecks++
			if healthChecks == 1 {
				return nil, errors.New("interrupted worker installation")
			}
			return nil, nil
		case strings.Contains(joined, "hostname -I"):
			return []byte("192.168.105.2\n"), nil
		case strings.Contains(joined, "server/node-token") || strings.Contains(joined, "k0s token create"):
			return []byte("synthetic-worker-token\n"), nil
		default:
			return nil, nil
		}
	}
	provisioner := NewClusterProvisioner(vm.NewWithStateDir(runner, vmRoot), runner, filepath.Join(root, "kubeconfigs"))
	if err := provisioner.writeClusterMetadata(ClusterRequest{
		Name: "dev", RuntimeName: "stable", Provider: provider, Version: metadataVersion, NodeGroups: []NodeGroupSpec{group},
	}); err != nil {
		t.Fatal(err)
	}
	return provisioner, runner, group
}

func TestAutoscalingWorkerPinsActualControllerRelease(t *testing.T) {
	for _, tc := range []struct {
		name, provider, metadata, requested, actual string
	}{
		{"k3s-empty", "k3s", "", "", "v1.35.4+k3s2"},
		{"k0s-empty", "k0s", "", "", "v1.35.4+k0s.1"},
		{"k3s-channel", "k3s", "stable", "stable", "v1.35.4+k3s2"},
		{"k0s-unpinned", "k0s", "v1.34.0", "v1.34.0", "v1.35.4+k0s.1"},
		{"controller-authoritative", "k3s", "v1.34.0+k3s1", "v1.36.0+k3s1", "v1.35.4+k3s2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, runner, group := autoscalingVersionFixture(t, tc.provider, tc.metadata, tc.actual, nil)
			if err := p.EnsureAutoscalingNode(context.Background(), "dev", group, tc.requested); err != nil {
				t.Fatal(err)
			}
			request, err := p.Configuration("dev")
			if err != nil || request.Version != tc.actual {
				t.Fatalf("controller release was not pinned in metadata: %q, %v", request.Version, err)
			}
			envKey := "INSTALL_K3S_VERSION="
			if tc.provider == "k0s" {
				envKey = "K0S_VERSION="
			}
			foundInstall, resolvedVersion := false, false
			for _, command := range runner.commands {
				joined := strings.Join(command.Args, " ")
				if strings.Contains(joined, "sudo "+tc.provider+" --version") {
					resolvedVersion = true
				}
				if slices.Contains(command.Args, "env") {
					if !resolvedVersion || !slices.Contains(command.Args, envKey+tc.actual) {
						t.Fatalf("worker installation was not pinned to the controller release: %v", command.Args)
					}
					foundInstall = true
				}
			}
			if !foundInstall {
				t.Fatal("interrupted worker installation was not exercised")
			}
		})
	}
}

func TestAutoscalingWorkerRefusesUnknownControllerRelease(t *testing.T) {
	for _, provider := range []string{"k3s", "k0s"} {
		for _, readFails := range []bool{false, true} {
			name := provider + "-invalid-release"
			var versionError error
			if readFails {
				name = provider + "-version-command-failed"
				versionError = errors.New("controller unavailable")
			}
			t.Run(name, func(t *testing.T) {
				p, runner, group := autoscalingVersionFixture(t, provider, "", "stable", versionError)
				err := p.EnsureAutoscalingNode(context.Background(), "dev", group, "")
				if err == nil || !strings.Contains(err.Error(), "no channel fallback") {
					t.Fatalf("unknown controller release did not fail closed: %v", err)
				}
				request, err := p.Configuration("dev")
				if err != nil || request.Version != "" {
					t.Fatalf("failed resolution changed metadata: %+v, %v", request, err)
				}
				for _, command := range runner.commands {
					joined := strings.Join(command.Args, " ")
					if slices.Contains(command.Args, "env") || strings.Contains(joined, "hostname -I") || strings.Contains(joined, "systemctl is-active") ||
						strings.HasSuffix(joined, "sudo k0s status") {
						t.Fatalf("worker mutated before controller release resolution: %v", command.Args)
					}
				}
			})
		}
	}
}
