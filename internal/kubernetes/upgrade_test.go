package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestK3sBundledStorageDoesNotRaceBootstrap(t *testing.T) {
	runner := newFakeRunner()
	waited := false
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		if strings.Contains(args, "wait --for=create storageclass/local-path") {
			waited = true
		}
		if strings.Contains(args, "get storageclass") {
			if !waited {
				t.Fatal("storage discovery ran before bundled k3s storage could register")
			}
			return []byte(`{"items":[{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}`), nil
		}
		if strings.Contains(args, localPathManifestURL) {
			t.Fatal("a second provisioner must not overwrite the bundled k3s provisioner's RBAC")
		}
		return nil, nil
	}
	p := NewClusterProvisioner(nil, runner, t.TempDir())
	if err := p.ensureClusterAddons(context.Background(), "/synthetic/kubeconfig", "porto-k3s-fixture", "k3s"); err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatal("bundled storage readiness was not awaited")
	}
}

func TestUpgradeVersionValidation(t *testing.T) {
	for _, test := range []struct {
		provider, current, target string
		valid                     bool
	}{
		{"k3s", "v1.35.2+k3s1", "v1.36.1+k3s1", true},
		{"k0s", "v1.36.1+k0s.0", "v1.36.1+k0s.1", true},
		{"kind", "v1.35.2", "v1.36.1", true},
		{"k3s", "v1.35.2+k3s1", "v1.37.1+k3s1", false},
		{"k3s", "v1.36.2+k3s1", "v1.36.1+k3s1", false},
		{"k0s", "v1.36.1+k0s.1", "v1.36.1+k0s.0", false},
		{"k3s", "v1.36.1+k3s1", "v1.36.2+k0s.0", false},
		{"kind", "v1.35.2", "latest", false},
		{"kind", "v1.35.2", "v1.36.1;touch /tmp/untrusted", false},
	} {
		t.Run(test.provider+"/"+test.current+"/"+test.target, func(t *testing.T) {
			err := validateUpgradeVersion(test.provider, test.current, test.target)
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, want valid %v", err, test.valid)
			}
		})
	}
}

func TestUpgradeRequiresConfirmationBeforeCommands(t *testing.T) {
	runner := newFakeRunner()
	p := NewClusterProvisioner(nil, runner, t.TempDir())
	_, err := p.UpgradeCluster(context.Background(), "dev", UpgradeRequest{Version: "v1.36.1+k3s1"})
	if err == nil || len(runner.commands) != 0 {
		t.Fatalf("upgrade without confirmation = %v, commands %v", err, runner.commands)
	}
}

func TestVMUpgradeControllerFirstAndOnlyCommitsAfterAllNodesReady(t *testing.T) {
	for _, provider := range []string{"k3s", "k0s"} {
		t.Run(provider, func(t *testing.T) {
			runner := newFakeRunner()
			p := NewClusterProvisioner(nil, runner, t.TempDir())
			oldVersion, target := "v1.35.2+k3s1", "v1.36.1+k3s1"
			if provider == "k0s" {
				oldVersion, target = "v1.35.2+k0s.0", "v1.36.1+k0s.0"
			}
			spec := ClusterRequest{Name: "dev", Provider: provider, Version: oldVersion, APIPort: 54321, NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}}}
			if err := p.writeClusterMetadata(spec); err != nil {
				t.Fatal(err)
			}
			var upgraded []string
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				switch {
				case strings.Contains(args, "get nodes -o json"):
					return upgradeNodeList(provider, oldVersion, target, upgraded), nil
				case strings.Contains(args, provider+" --version"):
					version := oldVersion
					for _, machine := range upgraded {
						if strings.Contains(args, machine) {
							version = target
						}
					}
					return []byte(version + "\n"), nil
				case strings.Contains(args, "porto-upgrade-node"):
					node := "porto-dev-server-1"
					if strings.Contains(args, "porto-dev-workers-1") {
						node = "porto-dev-workers-1"
						current, err := p.readClusterMetadata("dev")
						if err != nil || current.Version != oldVersion {
							t.Fatalf("version committed before workers: %+v, %v", current, err)
						}
					}
					upgraded = append(upgraded, node)
				case strings.Contains(args, "config view --raw"):
					return []byte(`{"apiVersion":"v1","kind":"Config","current-context":"default","clusters":[{"name":"default","cluster":{"server":"https://127.0.0.1:54321"}}],"contexts":[{"name":"default","context":{"cluster":"default","user":"default"}}],"users":[{"name":"default","user":{"token":"synthetic"}}]}`), nil
				case strings.Contains(args, "kubeconfig admin"), strings.Contains(args, "k3s.yaml"):
					return []byte("apiVersion: v1\ncurrent-context: default\nclusters:\n- cluster:\n    server: https://127.0.0.1:6443\n  name: default\ncontexts:\n- context:\n    cluster: default\n    user: default\n  name: default\nusers:\n- name: default\n  user:\n    token: synthetic\n"), nil
				}
				return nil, nil
			}
			_, err := p.UpgradeCluster(context.Background(), "dev", UpgradeRequest{Version: target, Confirm: true})
			if err != nil {
				t.Fatal(err)
			}
			current, err := p.readClusterMetadata("dev")
			if err != nil || current.Version != target || current.Phase != "" {
				t.Fatalf("final state = %+v, %v", current, err)
			}
			if strings.Join(upgraded, ",") != "porto-dev-server-1,porto-dev-workers-1" {
				t.Fatalf("upgrade order = %v", upgraded)
			}
		})
	}
}

func TestVMUpgradeFailureRetainsVersionAndRecoveryState(t *testing.T) {
	runner := newFakeRunner()
	p := NewClusterProvisioner(nil, runner, t.TempDir())
	spec := ClusterRequest{Name: "dev", Provider: "k3s", Version: "v1.35.2+k3s1"}
	if err := p.writeClusterMetadata(spec); err != nil {
		t.Fatal(err)
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Contains(strings.Join(command.Args, " "), "get nodes -o json") {
			return []byte(`{"items":[{"metadata":{"name":"porto-dev-server-1"},"status":{"nodeInfo":{"kubeletVersion":"v1.35.2+k3s1"},"conditions":[{"type":"Ready","status":"True"}]}}]}`), nil
		}
		if strings.Contains(strings.Join(command.Args, " "), "k3s --version") {
			return []byte("v1.35.2+k3s1\n"), nil
		}
		return nil, errors.New("synthetic upgrade failure")
	}
	_, err := p.UpgradeCluster(context.Background(), "dev", UpgradeRequest{Version: "v1.36.1+k3s1", Confirm: true})
	if err == nil {
		t.Fatal("expected failure")
	}
	current, err := p.readClusterMetadata("dev")
	if err != nil || current.Version != spec.Version || current.Phase != "upgrade-failed" || current.Upgrade == nil || current.Upgrade.Error == "" {
		t.Fatalf("failed state = %+v, %v", current, err)
	}
	if _, err := p.Configuration("../foreign"); err == nil {
		t.Fatal("invalid configuration identity accepted")
	}
	if _, err := os.Stat(p.clusterMetadataPath("dev")); err != nil {
		t.Fatal(err)
	}
}

func upgradeNodeList(provider, oldVersion, target string, upgraded []string) []byte {
	items := make([]map[string]any, 0, 2)
	for _, name := range []string{"porto-dev-server-1", "porto-dev-workers-1"} {
		version := oldVersion
		for _, done := range upgraded {
			if name == done {
				version = target
			}
		}
		if provider == "k0s" {
			name = "lima-" + name
		}
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": name},
			"status": map[string]any{
				"nodeInfo":   map[string]any{"kubeletVersion": version},
				"conditions": []map[string]any{{"type": "Ready", "status": "True"}},
			},
		})
	}
	data, _ := json.Marshal(map[string]any{"items": items})
	return data
}
