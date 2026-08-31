package vm

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type recordingRunner struct {
	mu       sync.Mutex
	commands []runtimes.Command
}

func (r *recordingRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	if len(command.Args) >= 2 && command.Args[0] == "list" && command.Args[1] == "--json" {
		return []byte(`{"name":"test-vm","status":"Running","arch":"aarch64","cpus":2,"memory":2147483648,"disk":21474836480,"sshLocalPort":60022}` + "\n"), nil
	}
	if len(command.Args) > 0 && command.Args[0] == "--version" {
		return []byte("limactl version 2.0.0"), nil
	}
	return nil, nil
}

func TestImageCatalogContainsRequiredDistributions(t *testing.T) {
	images := New(&recordingRunner{}).Images()
	required := []string{"Ubuntu", "CentOS Stream", "openSUSE", "NixOS", "Arch Linux", "Alpine Linux", "Kali Linux"}
	for _, distribution := range required {
		found := false
		for _, image := range images {
			if image.Distribution == distribution {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %s image", distribution)
		}
	}
}

func TestCreateUsesConfiguredResources(t *testing.T) {
	runner := &recordingRunner{}
	manager := New(runner)
	instance, err := manager.Create(context.Background(), CreateRequest{
		Name: "test-vm", Image: "ubuntu-24.04", CPUs: 4, MemoryMiB: 4096, DiskGiB: 30,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if instance.Name != "test-vm" {
		t.Fatalf("unexpected instance: %+v", instance)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) < 2 {
		t.Fatalf("expected create and list commands, got %+v", runner.commands)
	}
	createCommand := strings.Join(runner.commands[0].Args, " ")
	for _, expected := range []string{"--cpus 4", "--memory 4", "--disk 30", "template:ubuntu-24.04"} {
		if !strings.Contains(createCommand, expected) {
			t.Errorf("create command %q missing %q", createCommand, expected)
		}
	}
}

func TestStatusReportsLimaVersion(t *testing.T) {
	status := New(&recordingRunner{}).Status(context.Background())
	if !status.Available || !strings.Contains(status.Version, "2.0.0") {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestManagedInventoryFiltersUnownedLimaInstances(t *testing.T) {
	manager := NewWithStateDir(&recordingRunner{}, t.TempDir())
	if err := manager.writeMetadata(Metadata{Name: "test-vm", Kind: "standalone", Image: "ubuntu-24.04"}); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	instances, err := manager.List(context.Background())
	if err != nil {
		t.Fatalf("list managed VMs: %v", err)
	}
	if len(instances) != 1 || instances[0].Name != "test-vm" {
		t.Fatalf("unexpected managed VMs: %+v", instances)
	}
	if err := os.Remove(manager.metadataPath("test-vm")); err != nil {
		t.Fatal(err)
	}
	instances, err = manager.List(context.Background())
	if err != nil {
		t.Fatalf("list unmanaged VMs: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("unowned Lima instance leaked into Porto inventory: %+v", instances)
	}
}

func TestKubernetesNodeCreationReturnsManagedInstanceButIsNotStandalone(t *testing.T) {
	manager := NewWithStateDir(&recordingRunner{}, t.TempDir())
	instance, err := manager.CreateNode(context.Background(), CreateRequest{
		Name: "test-vm", Image: "ubuntu-24.04", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if instance.Name != "test-vm" {
		t.Fatalf("unexpected node instance: %+v", instance)
	}
	if err := manager.EnsureStandalone("test-vm"); err == nil || !strings.Contains(err.Error(), "kubernetes-node") {
		t.Fatalf("expected standalone ownership rejection, got %v", err)
	}
}

func TestSnapshotUsesCurrentLimaSyntax(t *testing.T) {
	runner := &recordingRunner{}
	manager := New(runner)
	if err := manager.CreateSnapshot(context.Background(), "test-vm", "before"); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	if err := manager.RestoreSnapshot(context.Background(), "test-vm", "before"); err != nil {
		t.Fatalf("restore snapshot: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	create := strings.Join(runner.commands[0].Args, " ")
	restore := strings.Join(runner.commands[1].Args, " ")
	if create != "snapshot create test-vm --tag before" {
		t.Fatalf("create command = %q", create)
	}
	if restore != "snapshot apply test-vm --tag before" {
		t.Fatalf("restore command = %q", restore)
	}
}
