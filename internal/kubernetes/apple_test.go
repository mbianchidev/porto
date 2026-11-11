package kubernetes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const appleClusterFixture = `[{"id":"porto-apple-demo","configuration":{"name":"porto-apple-demo","kubernetesVersion":"v1.35.5","creationDate":"2026-11-11T12:00:00Z","nodes":[{"name":"porto-apple-demo"},{"name":"porto-apple-demo-worker-1"}]},"status":{"state":"running","nodes":[{"name":"porto-apple-demo","status":"running"},{"name":"porto-apple-demo-worker-1","status":"running"}]}}]`

func newAppleProvisioner(t *testing.T, runner runtimes.Runner) *ClusterProvisioner {
	t.Helper()
	p := NewClusterProvisioner(nil, runner, t.TempDir())
	p.goos, p.goarch = "darwin", "arm64"
	p.appleLookPath = func(name string) (string, error) { return name, nil }
	return p
}

func TestAppleProviderUsesResolvedExecutableOutsideDaemonPATH(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name != "/synthetic/homebrew/bin/container" {
			return nil, errors.New("Apple CLI was not executed through its discovered path")
		}
		if slices.Equal(command.Args, []string{"--version"}) {
			return []byte("container CLI version 1.7.1"), nil
		}
		return []byte("[]"), nil
	}
	p := newAppleProvisioner(t, runner)
	p.appleLookPath = func(string) (string, error) { return "/synthetic/homebrew/bin/container", nil }
	if err := p.ensureAppleRuntimeAbsent(context.Background(), ClusterRequest{Name: "demo", Provider: "apple"}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyAppleClusterKeepsOwnershipAndPrivateKubeconfigWithoutMigration(t *testing.T) {
	externalConfig := filepath.Join(t.TempDir(), "external-config")
	if err := os.WriteFile(externalConfig, []byte("unrelated kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", externalConfig)
	created := true
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		switch command.Name {
		case "container":
			switch {
			case args == "--version":
				return []byte("container CLI version 1.7.1 (build: release)"), nil
			case args == "cluster list --format json":
				if !created {
					return []byte("[]"), nil
				}
				return []byte(appleClusterFixture), nil
			case args == "cluster delete --name porto-apple-demo":
				created = false
				return nil, nil
			}
		case "kubectl":
			switch {
			case strings.Contains(args, "config view --raw -o json"):
				return []byte(`{"apiVersion":"v1","kind":"Config","current-context":"porto-apple-demo","clusters":[{"name":"porto-apple-demo","cluster":{"server":"https://127.0.0.1:64430"}}],"contexts":[{"name":"porto-apple-demo","context":{"cluster":"porto-apple-demo","user":"porto-apple-demo"}}],"users":[{"name":"porto-apple-demo","user":{"token":"synthetic-token"}}]}`), nil
			case strings.Contains(args, "jsonpath={.clusters[0].cluster.server}"):
				return []byte("https://127.0.0.1:64430"), nil
			case strings.Contains(args, "get apiservice"):
				return []byte("true"), nil
			case strings.Contains(args, "get storageclass"):
				return []byte(`{"items":[{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}`), nil
			default:
				return nil, nil
			}
		}
		return nil, fmt.Errorf("unexpected command: %s %s", command.Name, args)
	}
	provisioner := newAppleProvisioner(t, runner)
	ctx := context.Background()
	spec := ClusterRequest{
		Name: "demo", Provider: "apple", Version: "v1.35.5", AppleCreatedAt: "2026-11-11T12:00:00Z",
		ControlPlane: MachineSpec{CPUs: 2, MemoryMiB: 2048},
		NodeGroups: []NodeGroupSpec{{
			Name: "workers", Count: 1, Machine: MachineSpec{CPUs: 4, MemoryMiB: 3072},
		}},
	}
	if err := provisioner.writeClusterMetadata(spec); err != nil {
		t.Fatal(err)
	}
	kubeconfig := []byte(`{"apiVersion":"v1","kind":"Config","current-context":"porto-demo","clusters":[{"name":"porto-demo","cluster":{"server":"https://127.0.0.1:64430"}}],"contexts":[{"name":"porto-demo","context":{"cluster":"porto-demo","user":"porto-demo"}}],"users":[{"name":"porto-demo","user":{"token":"synthetic-token"}}]}`)
	if err := writeFileAtomic(provisioner.clusterKubeconfigPath("demo"), kubeconfig); err != nil {
		t.Fatal(err)
	}
	items, err := provisioner.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("legacy inventory = %+v, error = %v", items, err)
	}
	cluster := items[0]
	if cluster.Provider != "apple" || cluster.Context != "porto-demo" ||
		cluster.State != "running" || cluster.Version != "v1.35.5" ||
		!slices.Equal(cluster.Nodes, []string{"porto-apple-demo", "porto-apple-demo-worker-1"}) {
		t.Fatalf("unexpected Apple cluster: %+v", cluster)
	}
	if err := provisioner.Rename(ctx, "demo", "renamed"); err != nil {
		t.Fatal(err)
	}
	clusters, err := provisioner.List(ctx)
	if err != nil || len(clusters) != 1 || clusters[0].Name != "renamed" ||
		clusters[0].State != "running" || clusters[0].Context != "porto-renamed" {
		t.Fatalf("renamed Apple cluster = %+v, error = %v", clusters, err)
	}
	if err := provisioner.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("Apple cluster was not deleted")
	}
	clusters, err = provisioner.List(ctx)
	if err != nil || len(clusters) != 0 {
		t.Fatalf("deleted cluster remains: %+v, error = %v", clusters, err)
	}
	data, err := os.ReadFile(externalConfig)
	if err != nil || string(data) != "unrelated kubeconfig" {
		t.Fatalf("external kubeconfig changed: %q, error = %v", data, err)
	}
	for _, command := range runner.commands {
		if command.Name != "container" && command.Name != "kubectl" {
			t.Fatalf("Apple provider used another runtime: %+v", command)
		}
		if command.Name == "container" && len(command.Args) > 1 && command.Args[1] == "create" {
			t.Fatalf("legacy cluster was silently recreated: %+v", command)
		}
	}
}

func TestAppleUnsupportedLifecycleDoesNotChangeClusterOrTouchAnotherRuntime(t *testing.T) {
	tests := map[string]func(*ClusterProvisioner) error{
		"start": func(p *ClusterProvisioner) error {
			_, err := p.Start(context.Background(), "demo")
			return err
		},
		"stop": func(p *ClusterProvisioner) error {
			return p.SetRunning(context.Background(), "demo", false)
		},
		"scale": func(p *ClusterProvisioner) error {
			return p.ScaleNodeGroup(context.Background(), "demo", NodeGroupSpec{Name: "workers", Count: 2}, "")
		},
		"upgrade": func(p *ClusterProvisioner) error {
			_, err := p.UpgradeCluster(context.Background(), "demo", UpgradeRequest{Version: "v1.36.1", Confirm: true})
			return err
		},
		"upgrade recovery": func(p *ClusterProvisioner) error {
			return p.RecoverUpgrade(context.Background(), "demo", true)
		},
	}
	for name, action := range tests {
		t.Run(name, func(t *testing.T) {
			runner := newFakeRunner()
			p := NewClusterProvisioner(nil, runner, t.TempDir())
			request := ClusterRequest{Name: "demo", Provider: "apple", Version: "v1.35.5"}
			if err := p.writeClusterMetadata(request); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(p.clusterMetadataPath("demo"))
			if err != nil {
				t.Fatal(err)
			}
			err = action(p)
			if err == nil || !strings.Contains(err.Error(), "Apple Cluster does not support") {
				t.Fatalf("unsupported operation returned %v", err)
			}
			after, readErr := os.ReadFile(p.clusterMetadataPath("demo"))
			if readErr != nil || !bytes.Equal(before, after) || len(runner.commands) != 0 {
				t.Fatalf("unsupported action changed state or invoked a runtime: %s, %+v, %v", after, runner.commands, readErr)
			}
		})
	}
}

func TestAppleCreationRejectsOtherHostsBeforeWritingState(t *testing.T) {
	for _, host := range [][2]string{{"darwin", "amd64"}, {"linux", "arm64"}, {"windows", "arm64"}} {
		t.Run(strings.Join(host[:], "-"), func(t *testing.T) {
			runner := newFakeRunner()
			p := NewClusterProvisioner(nil, runner, t.TempDir())
			p.goos, p.goarch = host[0], host[1]
			_, err := p.Create(context.Background(), ClusterRequest{Name: "demo", Provider: "apple"})
			if err == nil || !strings.Contains(err.Error(), "Apple silicon") {
				t.Fatalf("wrong host returned %v", err)
			}
			entries, err := os.ReadDir(p.kubeconfigRoot)
			if err != nil || len(entries) != 0 || len(runner.commands) != 0 {
				t.Fatalf("unsupported host changed state: %+v, %+v, %v", entries, runner.commands, err)
			}
		})
	}
}

func TestAppleDeletionRefusesRecreatedOrUnidentifiedClusters(t *testing.T) {
	for _, test := range []struct {
		name, identity, inventory, message string
	}{
		{"missing ownership", "", appleClusterFixture, "ownership"},
		{"recreated cluster", "2026-11-10T12:00:00Z", appleClusterFixture, "ownership"},
		{"missing inventory", "2026-11-11T12:00:00Z", "null", "inventory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newFakeRunner()
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				switch strings.Join(command.Args, " ") {
				case "--version":
					return []byte("container CLI version 1.7.1"), nil
				case "cluster list --format json":
					return []byte(test.inventory), nil
				default:
					return nil, errors.New("unexpected mutation")
				}
			}
			p := newAppleProvisioner(t, runner)
			if err := p.writeClusterMetadata(ClusterRequest{
				Name: "demo", Provider: "apple", AppleCreatedAt: test.identity,
				NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}},
			}); err != nil {
				t.Fatal(err)
			}
			err := p.Delete(context.Background(), "demo")
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("unsafe deletion returned %v", err)
			}
			if _, err := p.readClusterMetadata("demo"); err != nil {
				t.Fatalf("failed deletion lost ownership metadata: %v", err)
			}
			for _, command := range runner.commands {
				if len(command.Args) > 1 && command.Args[1] == "delete" {
					t.Fatalf("deleted a different resource: %+v", command)
				}
			}
		})
	}
}

func TestClusterAddonsSupplyDefaultStorageWithoutReplacingAnExistingDefault(t *testing.T) {
	for _, existingDefault := range []bool{false, true} {
		t.Run(fmt.Sprint("existing-default-", existingDefault), func(t *testing.T) {
			defaultStorage := existingDefault
			installed := false
			run := func(_ string, _ []byte, args ...string) ([]byte, error) {
				if len(args) > 0 && args[0] == "get" {
					if existingDefault {
						return []byte(`{"items":[{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}]}`), nil
					}
					return []byte(`{"items":[]}`), nil
				}
				if existingDefault {
					return nil, errors.New("an existing default storage class must not be changed")
				}
				if len(args) > 0 && args[0] == "apply" {
					installed = true
				}
				if len(args) > 0 && args[0] == "annotate" && installed &&
					slices.Contains(args, "storageclass.kubernetes.io/is-default-class=true") {
					defaultStorage = true
				}
				return nil, nil
			}
			if err := ensurePersistentStorage(run); err != nil {
				t.Fatal(err)
			}
			if !defaultStorage {
				t.Fatal("PVCs without storageClassName cannot bind: Porto did not provide a default storage class")
			}
		})
	}
}

func TestAppleKubernetesStatusSeparatesWarningsWithoutDroppingExecDiagnostics(t *testing.T) {
	runner := newFakeRunner()
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if slices.Contains(command.Args, "exec") {
			output := "synthetic stdout\n"
			if !command.StdoutOnly {
				output += "synthetic process stderr\n"
			}
			return []byte(output), nil
		}
		output := `{"clientVersion":{"gitVersion":"v1.37.2"},"serverVersion":{"gitVersion":"v1.35.5"}}`
		if !command.StdoutOnly {
			output += "\nWARNING: synthetic kubectl version skew\n"
		}
		return []byte(output), nil
	}
	p := NewClusterProvisioner(nil, runner, t.TempDir())
	if err := writeFileAtomic(p.clusterKubeconfigPath("demo"), []byte("apiVersion: v1\nkind: Config\n")); err != nil {
		t.Fatal(err)
	}
	manager := NewWithKubeconfigRoot(runner, p.kubeconfigRoot)
	if status := manager.Status(context.Background(), "porto-demo"); !status.Available || status.ServerVersion != "v1.35.5" {
		t.Fatalf("kubectl warning corrupted native status: %+v", status)
	}
	output, err := manager.Exec(context.Background(), "porto-demo", "default", "fixture", "app",
		[]string{"synthetic-command", "-o", "json"}, nil)
	if err != nil || string(output) != "synthetic stdout\nsynthetic process stderr\n" {
		t.Fatalf("container diagnostics were dropped: %q, error = %v", output, err)
	}
}

func TestAppleImageImportStreamsArchiveIntoEveryOwnedNode(t *testing.T) {
	for _, failWorker := range []bool{false, true} {
		t.Run(fmt.Sprint("worker-failure-", failWorker), func(t *testing.T) {
			runner := newFakeRunner()
			var archivePath string
			var imported []string
			runner.handler = func(command runtimes.Command) ([]byte, error) {
				args := strings.Join(command.Args, " ")
				if command.Name != "container" {
					return nil, fmt.Errorf("image import used another runtime: %s", command.Name)
				}
				switch {
				case args == "--version":
					return []byte("container CLI version 1.7.1"), nil
				case args == "cluster list --format json":
					return []byte(appleClusterFixture), nil
				case strings.HasPrefix(args, "image save "):
					index := slices.Index(command.Args, "--output")
					if index < 0 || !strings.Contains(args, "--platform linux/arm64") {
						return nil, errors.New("image export must select a private file and native platform")
					}
					archivePath = command.Args[index+1]
					return nil, os.WriteFile(archivePath, []byte("synthetic OCI archive"), 0o600)
				case strings.HasPrefix(args, "exec --interactive "):
					if command.StdinReader == nil || command.Stdin != nil || !strings.Contains(args, "ctr --namespace k8s.io images import") {
						return nil, errors.New("image import must stream into Kubernetes containerd")
					}
					data, err := io.ReadAll(command.StdinReader)
					if err != nil || string(data) != "synthetic OCI archive" {
						return nil, fmt.Errorf("incorrect archive: %q, error = %v", data, err)
					}
					imported = append(imported, command.Args[2])
					if failWorker && strings.Contains(command.Args[2], "worker") {
						return nil, errors.New("synthetic worker import failure")
					}
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected command: %s", args)
				}
			}
			p := newAppleProvisioner(t, runner)
			if err := p.writeClusterMetadata(ClusterRequest{
				Name: "demo", Provider: "apple", AppleCreatedAt: "2026-11-11T12:00:00Z",
				NodeGroups: []NodeGroupSpec{{Name: "workers", Count: 1}},
			}); err != nil {
				t.Fatal(err)
			}
			err := p.ImportImage(context.Background(), "demo", "synthetic/app:dev")
			if failWorker {
				if err == nil || !strings.Contains(err.Error(), "porto-apple-demo-worker-1") {
					t.Fatalf("partial import was not reported: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(imported, []string{"porto-apple-demo", "porto-apple-demo-worker-1"}) {
				t.Fatalf("incorrect imported nodes: %v", imported)
			}
			if archivePath == "" {
				t.Fatal("no image archive was exported")
			}
			if _, err := os.Stat(archivePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("private image archive was not cleaned: %v", err)
			}
		})
	}
}
