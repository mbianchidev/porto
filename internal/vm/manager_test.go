package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type recordingRunner struct {
	mu           sync.Mutex
	commands     []runtimes.Command
	vmType       string
	status       string
	failSnapshot bool
	listPrefix   string
	invalidList  bool
}

type brokenInstanceRunner struct {
	mu               sync.Mutex
	commands         []runtimes.Command
	recovered        bool
	deleted          bool
	forceDeleteError bool
	listStatus       string
	startOutput      string
	startError       error
}

type blockingResourceRunner struct{}

func (blockingResourceRunner) Run(ctx context.Context, _ runtimes.Command) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (r *brokenInstanceRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	joined := strings.Join(command.Args, " ")
	switch joined {
	case "list --json":
		if r.deleted {
			return nil, nil
		}
		status := r.listStatus
		if status == "" {
			status = "Broken"
		}
		if r.recovered && r.listStatus == "" {
			status = "Stopped"
		}
		return []byte(fmt.Sprintf(
			`{"name":"test-vm","status":%q,"vmType":"vz","arch":"aarch64","cpus":2,"memory":2147483648,"disk":10737418240}`+"\n",
			status,
		)), nil
	case "start test-vm":
		if r.startError != nil {
			return []byte(r.startOutput), r.startError
		}
		if !r.recovered {
			return []byte("Instance \"test-vm\" has configuration errors: dial unix ha.sock: connect: connection refused"), errors.New("exit status 1")
		}
		return nil, nil
	case "stop test-vm":
		return []byte("expected status `Running`, got `Broken`"), errors.New("exit status 1")
	case "stop --force test-vm":
		r.recovered = true
		return nil, nil
	case "shell test-vm -- true":
		return nil, nil
	case "delete test-vm":
		return []byte("expected status `Stopped`, got `Broken`"), errors.New("exit status 1")
	case "delete --force test-vm":
		r.deleted = true
		if r.forceDeleteError {
			return []byte("instance deleted; network reconciliation failed"), errors.New("exit status 1")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %s %s", command.Name, joined)
	}
}

func (r *recordingRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	if len(command.Args) >= 2 && command.Args[0] == "list" && command.Args[1] == "--json" {
		if r.invalidList {
			return []byte("{invalid"), nil
		}
		vmType := r.vmType
		if vmType == "" {
			vmType = "qemu"
		}
		status := r.status
		if status == "" {
			status = "Running"
		}
		return []byte(r.listPrefix + fmt.Sprintf(
			`{"name":"test-vm","status":%q,"vmType":%q,"arch":"aarch64","cpus":2,"memory":2147483648,"disk":21474836480,"sshLocalPort":60022}`+"\n",
			status,
			vmType,
		)), nil
	}
	if len(command.Args) > 0 && command.Args[0] == "snapshot" && r.failSnapshot {
		return []byte("snapshot failed"), errors.New("exit status 1")
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

func TestImageCatalogExplainsRollingAndInstallerImages(t *testing.T) {
	images := New(&recordingRunner{}).Images()
	arch, archFound := imageByID(images, "archlinux")
	if !archFound {
		t.Fatal("missing Arch Linux image")
	}
	if arch.Version == "Rolling" || !supportsArchitecture(arch, "x86_64") || supportsArchitecture(arch, "aarch64") {
		t.Fatalf("unexpected Arch Linux catalog entry: %+v", arch)
	}
	kali, kaliFound := imageByID(images, "kali")
	if !kaliFound {
		t.Fatal("missing Kali Linux image")
	}
	if kali.Available || !strings.Contains(kali.Message, "cloud-init") || !strings.Contains(kali.Version, "2026.2") {
		t.Fatalf("unexpected Kali Linux catalog entry: %+v", kali)
	}
}

func TestCreateUsesConfiguredResources(t *testing.T) {
	runner := &recordingRunner{}
	manager := New(runner)
	instance, err := manager.Create(context.Background(), CreateRequest{
		Name: "test-vm", Image: "ubuntu-24.04", VMType: "qemu", CPUs: 4, MemoryMiB: 4096, DiskGiB: 30,
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
	for _, expected := range []string{"--vm-type qemu", "--cpus 4", "--memory 4", "--disk 30", "template:ubuntu-24.04"} {
		if !strings.Contains(createCommand, expected) {
			t.Errorf("create command %q missing %q", createCommand, expected)
		}
	}
}

func TestCreateRejectsUnsupportedVMType(t *testing.T) {
	_, err := New(&recordingRunner{}).Create(context.Background(), CreateRequest{
		Name: "test-vm", Image: "ubuntu-24.04", VMType: "virtualbox",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported VM type") {
		t.Fatalf("create error = %v", err)
	}
}

func TestCreateConfigIncludesRequestedVMType(t *testing.T) {
	manager := New(&recordingRunner{})
	image, ok := manager.image("ubuntu-24.04")
	if !ok {
		t.Fatal("missing Ubuntu image")
	}
	path, err := manager.writeCreateConfig(CreateRequest{
		Name: "test-vm", VMType: "qemu", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Network: "user-v2",
	}, image)
	if err != nil {
		t.Fatalf("write create config: %v", err)
	}
	defer os.Remove(path)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read create config: %v", err)
	}
	if !strings.Contains(string(content), `vmType: "qemu"`) {
		t.Fatalf("create config missing VM type:\n%s", content)
	}
}

func TestCreateUsesOnlySupportedImageArchitecture(t *testing.T) {
	runner := &recordingRunner{}
	manager := New(runner)
	if _, err := manager.Create(context.Background(), CreateRequest{
		Name: "test-vm", Image: "archlinux", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	createCommand := strings.Join(runner.commands[0].Args, " ")
	if !strings.Contains(createCommand, "--arch x86_64") {
		t.Fatalf("create command %q did not select x86_64", createCommand)
	}
}

func TestStatusReportsLimaVersion(t *testing.T) {
	status := New(&recordingRunner{}).Status(context.Background())
	if !status.Available || !strings.Contains(status.Version, "2.0.0") {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestResourceStatsUsesShortDedicatedTimeout(t *testing.T) {
	manager := New(blockingResourceRunner{})
	manager.resourceTimeout = 10 * time.Millisecond
	started := time.Now()
	_, err := manager.ResourceStats(context.Background(), "test-vm")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("ResourceStats error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ResourceStats blocked for %s", elapsed)
	}
}

func TestLimaCommandsIncludeDynamicallyDiscoveredQEMU(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew QEMU path behavior is macOS-specific")
	}
	runner := &recordingRunner{}
	manager := New(runner)
	manager.getenv = func(string) string { return "" }
	manager.lookPath = func(name string) (string, error) {
		if name == "qemu-system-aarch64" || name == "qemu-system-x86_64" {
			return "/opt/homebrew/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	if err := manager.Start(context.Background(), "test-vm"); err != nil {
		t.Fatalf("start VM: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name != "limactl" {
			continue
		}
		environment := strings.Join(command.Env, "\n")
		if !strings.Contains(environment, "PATH=/opt/homebrew/bin") ||
			!strings.Contains(environment, "QEMU_HOME=/opt/homebrew") {
			t.Fatalf("Lima command is missing QEMU environment: %+v", command)
		}
	}
}

func TestLimaCommandsIncludeConfiguredQEMUOverride(t *testing.T) {
	runner := &recordingRunner{}
	manager := New(runner)
	manager.getenv = func(name string) string {
		if name == "QEMU_SYSTEM_AARCH64" {
			return "/Applications/QEMU/bin/qemu-system-aarch64"
		}
		return ""
	}
	manager.lookPath = func(name string) (string, error) {
		if name == "/Applications/QEMU/bin/qemu-system-aarch64" {
			return name, nil
		}
		return "", errors.New("not found")
	}
	if err := manager.Start(context.Background(), "test-vm"); err != nil {
		t.Fatalf("start VM: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if command.Name != "limactl" {
			continue
		}
		environment := strings.Join(command.Env, "\n")
		if !strings.Contains(environment, "QEMU_SYSTEM_AARCH64=/Applications/QEMU/bin/qemu-system-aarch64") {
			t.Fatalf("Lima command is missing QEMU override: %+v", command)
		}
	}
}

func TestStartRecoversBrokenLimaInstance(t *testing.T) {
	runner := &brokenInstanceRunner{}
	if err := New(runner).Start(context.Background(), "test-vm"); err != nil {
		t.Fatalf("start broken VM: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var commands []string
	for _, command := range runner.commands {
		commands = append(commands, strings.Join(command.Args, " "))
	}
	want := []string{
		"start test-vm",
		"list --json",
		"stop --force test-vm",
		"start test-vm",
		"shell test-vm -- true",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recovery commands:\n%s\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestStopRecoversBrokenLimaInstance(t *testing.T) {
	runner := &brokenInstanceRunner{}
	if err := New(runner).Stop(context.Background(), "test-vm"); err != nil {
		t.Fatalf("stop broken VM: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var commands []string
	for _, command := range runner.commands {
		commands = append(commands, strings.Join(command.Args, " "))
	}
	want := []string{"stop test-vm", "list --json", "stop --force test-vm"}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recovery commands:\n%s\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestDeleteRecoversBrokenLimaInstance(t *testing.T) {
	runner := &brokenInstanceRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.writeMetadata(Metadata{Name: "test-vm", Kind: "standalone", Image: "ubuntu-24.04"}); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	if err := manager.Delete(context.Background(), "test-vm", false); err != nil {
		t.Fatalf("delete broken VM: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var commands []string
	for _, command := range runner.commands {
		commands = append(commands, strings.Join(command.Args, " "))
	}
	want := []string{"delete test-vm", "list --json", "delete --force test-vm"}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("recovery commands:\n%s\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestStartDoesNotForceStopRunningInstanceAfterUnrelatedFailure(t *testing.T) {
	runner := &brokenInstanceRunner{
		listStatus:  "Running",
		startOutput: "image download failed",
		startError:  errors.New("exit status 1"),
	}
	err := New(runner).Start(context.Background(), "test-vm")
	if err == nil || !strings.Contains(err.Error(), "image download failed") {
		t.Fatalf("unexpected start error: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if strings.Join(command.Args, " ") == "stop --force test-vm" {
			t.Fatalf("running VM was force-stopped: %+v", runner.commands)
		}
	}
}

func TestDeleteRemovesMetadataWhenLimaDeletedBeforeReportingError(t *testing.T) {
	runner := &brokenInstanceRunner{forceDeleteError: true}
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.writeMetadata(Metadata{Name: "test-vm", Kind: "standalone", Image: "ubuntu-24.04"}); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	err := manager.Delete(context.Background(), "test-vm", false)
	if err == nil || !strings.Contains(err.Error(), "network reconciliation failed") {
		t.Fatalf("unexpected delete error: %v", err)
	}
	if _, statErr := os.Stat(manager.metadataPath("test-vm")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("stale VM metadata remains after deletion: %v", statErr)
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

func TestListIgnoresLimaDiagnostics(t *testing.T) {
	runner := &recordingRunner{
		listPrefix: `time="2026-09-01T22:00:00+02:00" level=warning msg="instance has errors"` + "\n",
	}
	instances, err := New(runner).List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(instances) != 1 || instances[0].Name != "test-vm" {
		t.Fatalf("instances = %+v", instances)
	}
}

func TestCreateCleansUpWhenFinalInspectionFails(t *testing.T) {
	runner := &recordingRunner{invalidList: true}
	_, err := New(runner).Create(context.Background(), CreateRequest{
		Name: "test-vm", Image: "ubuntu-24.04", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20,
	})
	if err == nil || !strings.Contains(err.Error(), "decode Lima instance") {
		t.Fatalf("create error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	deleted := false
	for _, command := range runner.commands {
		if strings.Join(command.Args, " ") == "delete --force test-vm" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("failed create was not cleaned up: %+v", runner.commands)
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

func TestKubernetesNodeOwnershipSurvivesClusterRename(t *testing.T) {
	manager := NewWithStateDir(&recordingRunner{}, t.TempDir())
	if _, err := manager.CreateNode(context.Background(), CreateRequest{
		Name: "test-vm", Owner: "dev", Image: "ubuntu-24.04", CPUs: 2, MemoryMiB: 2048, DiskGiB: 20,
	}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := manager.RenameKubernetesOwner("dev", "prod"); err != nil {
		t.Fatalf("rename node owner: %v", err)
	}
	names, err := manager.KubernetesNodeNames("prod")
	if err != nil {
		t.Fatalf("list node ownership: %v", err)
	}
	if !reflect.DeepEqual(names, []string{"test-vm"}) {
		t.Fatalf("owned nodes = %v", names)
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
	var snapshotCommands []string
	for _, command := range runner.commands {
		if len(command.Args) > 0 && command.Args[0] == "snapshot" {
			snapshotCommands = append(snapshotCommands, strings.Join(command.Args, " "))
		}
	}
	if len(snapshotCommands) != 2 {
		t.Fatalf("snapshot commands = %q", snapshotCommands)
	}
	create := snapshotCommands[0]
	restore := snapshotCommands[1]
	if create != "snapshot create test-vm --tag before" {
		t.Fatalf("create command = %q", create)
	}
	if restore != "snapshot apply test-vm --tag before" {
		t.Fatalf("restore command = %q", restore)
	}
}

func TestSnapshotStopsAndRestartsRunningVM(t *testing.T) {
	runner := &recordingRunner{}
	err := New(runner).CreateSnapshot(context.Background(), "test-vm", "before")
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	var commands []string
	for _, command := range runner.commands {
		commands = append(commands, strings.Join(command.Args, " "))
	}
	want := []string{
		"list --json",
		"stop test-vm",
		"snapshot create test-vm --tag before",
		"start test-vm",
		"shell test-vm -- true",
	}
	if strings.Join(commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("snapshot commands:\n%s\nwant:\n%s", strings.Join(commands, "\n"), strings.Join(want, "\n"))
	}
}

func TestSnapshotLeavesStoppedVMStopped(t *testing.T) {
	runner := &recordingRunner{status: "Stopped"}
	if err := New(runner).CreateSnapshot(context.Background(), "test-vm", "before"); err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if len(command.Args) > 0 && (command.Args[0] == "stop" || command.Args[0] == "start") {
			t.Fatalf("stopped VM lifecycle changed: %+v", runner.commands)
		}
	}
}

func TestSnapshotRestartsRunningVMAfterFailure(t *testing.T) {
	runner := &recordingRunner{failSnapshot: true}
	err := New(runner).CreateSnapshot(context.Background(), "test-vm", "before")
	if err == nil || !strings.Contains(err.Error(), "snapshot failed") {
		t.Fatalf("create snapshot error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	started := false
	for _, command := range runner.commands {
		if len(command.Args) > 0 && command.Args[0] == "start" {
			started = true
		}
	}
	if !started {
		t.Fatalf("running VM was not restarted after snapshot failure: %+v", runner.commands)
	}
}

func TestListReportsSnapshotCapability(t *testing.T) {
	tests := []struct {
		name      string
		vmType    string
		supported bool
		message   string
	}{
		{name: "qemu", vmType: "qemu", supported: true},
		{name: "vz", vmType: "vz", message: "QEMU"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instances, err := New(&recordingRunner{vmType: test.vmType}).List(context.Background())
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(instances) != 1 {
				t.Fatalf("instances = %+v", instances)
			}
			instance := instances[0]
			if instance.VMType != test.vmType || instance.SnapshotSupported != test.supported {
				t.Fatalf("unexpected snapshot capability: %+v", instance)
			}
			if test.message != "" && !strings.Contains(instance.SnapshotMessage, test.message) {
				t.Fatalf("snapshot message = %q, want %q", instance.SnapshotMessage, test.message)
			}
		})
	}
}

func TestSnapshotRejectsUnsupportedDriver(t *testing.T) {
	runner := &recordingRunner{vmType: "vz"}
	err := New(runner).CreateSnapshot(context.Background(), "test-vm", "before")
	if err == nil || !strings.Contains(err.Error(), "unsupported") || !strings.Contains(err.Error(), "QEMU") {
		t.Fatalf("create snapshot error = %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if len(command.Args) > 0 && command.Args[0] == "snapshot" {
			t.Fatalf("unsupported snapshot command was executed: %+v", command)
		}
	}
}

func TestSnapshotNameValidationUsesSnapshotTerminology(t *testing.T) {
	err := New(&recordingRunner{}).CreateSnapshot(context.Background(), "test-vm", "Before Upgrade")
	if err == nil || !strings.Contains(err.Error(), "snapshot name must match") {
		t.Fatalf("create snapshot error = %v", err)
	}
}

func imageByID(images []Image, id string) (Image, bool) {
	for _, image := range images {
		if image.ID == id {
			return image, true
		}
	}
	return Image{}, false
}
