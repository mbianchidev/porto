package vm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type vmRunnerFunc func(context.Context, runtimes.Command) ([]byte, error)

func (f vmRunnerFunc) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	return f(ctx, command)
}

func TestCreateNodeDisablesUnneededHostIntegration(t *testing.T) {
	for _, network := range []string{"", "user-v2"} {
		t.Run("network="+network, func(t *testing.T) {
			var createCommand runtimes.Command
			created := false
			runner := vmRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
				switch command.Args[0] {
				case "create":
					createCommand = command
					created = true
				case "list":
					if created {
						return json.Marshal(map[string]any{"name": "synthetic-node", "status": "Running", "vmType": "qemu"})
					}
				}
				return nil, nil
			})
			manager := NewWithStateDir(runner, t.TempDir())
			_, err := manager.CreateNode(context.Background(), CreateRequest{
				Name: "synthetic-node", Owner: "synthetic-cluster", Image: "ubuntu-24.04",
				CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Network: network, Start: true,
			})
			if err != nil {
				t.Fatalf("create node: %v", err)
			}
			if !slices.Contains(createCommand.Args, "--mount-none") {
				t.Errorf("Kubernetes node inherits host mounts requiring Windows reverse-sshfs: %v", createCommand.Args)
			}
			if !slices.Contains(createCommand.Args, "--containerd=none") {
				t.Errorf("Kubernetes node installs Lima containerd instead of only its distribution's runtime: %v", createCommand.Args)
			}
		})
	}
}

func TestCreateNodeBudgetIncludesImagePreparation(t *testing.T) {
	var startBudgets []time.Duration
	created := false
	runner := vmRunnerFunc(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
		switch command.Args[0] {
		case "create":
			created = true
		case "start":
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("Lima start has no deadline")
			}
			startBudgets = append(startBudgets, time.Until(deadline))
		case "list":
			if created {
				return json.Marshal(map[string]any{"name": "synthetic-node", "status": "Running", "vmType": "qemu"})
			}
		}
		return nil, nil
	})
	manager := NewWithStateDir(runner, t.TempDir())
	_, err := manager.CreateNode(context.Background(), CreateRequest{
		Name: "synthetic-node", Owner: "synthetic-cluster", Image: "ubuntu-24.04",
		CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Network: "user-v2", Start: true,
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := manager.Stop(context.Background(), "synthetic-node"); err != nil {
		t.Fatalf("stop node: %v", err)
	}
	if err := manager.Start(context.Background(), "synthetic-node"); err != nil {
		t.Fatalf("restart node: %v", err)
	}
	if len(startBudgets) != 2 {
		t.Fatalf("start budgets = %v, want first boot and restart", startBudgets)
	}
	for index, want := range []time.Duration{20 * time.Minute, 5 * time.Minute} {
		if got := startBudgets[index]; got < want-time.Second || got > want {
			t.Errorf("start %d budget = %s, want %s (initial image preparation must not consume the boot deadline)", index, got, want)
		}
	}
}

func TestStartDeadlinePreservesDiagnosticsWithoutRecovery(t *testing.T) {
	var commands []runtimes.Command
	const lastDiagnostic = "Downloading Ubuntu image: synthetic TLS handshake stalled"
	output := []byte("old progress must be truncated\n" + strings.Repeat("progress\n", 10_000) + lastDiagnostic)
	runner := vmRunnerFunc(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
		commands = append(commands, command)
		if command.Args[0] == "start" {
			<-ctx.Done()
			return output, ctx.Err()
		}
		if command.Args[0] == "list" {
			return json.Marshal(map[string]any{"name": "synthetic-node", "status": "Broken", "vmType": "qemu"})
		}
		return nil, nil
	})
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.writeMetadata(Metadata{Name: "synthetic-node", Kind: "kubernetes-node", Owner: "synthetic-cluster"}); err != nil {
		t.Fatal(err)
	}

	err := manager.start(context.Background(), "synthetic-node", 15*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline cause lost: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), lastDiagnostic) {
		t.Errorf("last startup diagnostic lost: %v", err)
	}
	if err != nil && (strings.Contains(err.Error(), "old progress must be truncated") || len(err.Error()) > 64*1024+512) {
		t.Errorf("startup diagnostic was not bounded: %d bytes", len(err.Error()))
	}
	if len(commands) != 2 || !slices.Equal(commands[0].Args, []string{"list", "--json", "synthetic-node"}) || commands[1].Args[0] != "start" {
		var arguments [][]string
		for _, command := range commands {
			arguments = append(arguments, command.Args)
		}
		t.Errorf("timed out boot was retried or force-stopped: %v", arguments)
	}
}

func TestNodeActionsNeverRecoverUnownedBrokenInstance(t *testing.T) {
	for _, action := range []string{"start", "stop", "delete"} {
		t.Run(action, func(t *testing.T) {
			var commands [][]string
			runner := vmRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
				commands = append(commands, command.Args)
				if command.Args[0] == "list" {
					return json.Marshal(map[string]any{"name": "synthetic-foreign", "status": "Broken", "vmType": "qemu"})
				}
				return nil, nil
			})
			manager := NewWithStateDir(runner, t.TempDir())
			var err error
			switch action {
			case "start":
				err = manager.Start(context.Background(), "synthetic-foreign")
			case "stop":
				err = manager.Stop(context.Background(), "synthetic-foreign")
			case "delete":
				err = manager.Delete(context.Background(), "synthetic-foreign", true)
			}
			if err == nil || !strings.Contains(err.Error(), "not managed by Porto") {
				t.Errorf("ownership rejection was lost: %v", err)
			}
			if len(commands) != 0 {
				t.Errorf("unowned instance entered lifecycle/recovery commands: %v", commands)
			}
		})
	}
}

func TestCreateNodeChecksExactInstanceBeforeCreate(t *testing.T) {
	var commands [][]string
	runner := vmRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		commands = append(commands, command.Args)
		if command.Args[0] == "list" {
			return []byte("{\"name\":\"synthetic-node\",\"status\":\"Broken\",\"vmType\":\"qemu\"}\n" +
				"{\"name\":\"synthetic-node-neighbor\",\"status\":\"Running\",\"vmType\":\"qemu\"}\n"), nil
		}
		return nil, nil
	})
	manager := NewWithStateDir(runner, t.TempDir())

	_, err := manager.CreateNode(context.Background(), CreateRequest{
		Name: "synthetic-node", Owner: "synthetic-cluster", Image: "ubuntu-24.04",
		CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Network: "user-v2", Start: true,
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("existing Lima instance was not rejected: %v", err)
	}
	if len(commands) != 1 || !slices.Equal(commands[0], []string{"list", "--json", "synthetic-node"}) {
		t.Errorf("expected only an exact-name read before rejecting creation, got %v", commands)
	}
	if _, err := os.Stat(manager.metadataPath("synthetic-node")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("foreign instance received Porto ownership metadata: %v", err)
	}
}

func TestStartDoesNotImplicitlyCreateMissingOwnedInstance(t *testing.T) {
	var commands [][]string
	runner := vmRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		commands = append(commands, command.Args)
		return nil, nil
	})
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.writeMetadata(Metadata{Name: "synthetic-missing", Kind: "kubernetes-node", Owner: "synthetic-cluster"}); err != nil {
		t.Fatal(err)
	}

	err := manager.Start(context.Background(), "synthetic-missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing VM was not rejected: %v", err)
	}
	if len(commands) != 1 || !slices.Equal(commands[0], []string{"list", "--json", "synthetic-missing"}) {
		t.Errorf("missing VM reached limactl start (which implicitly creates default instances): %v", commands)
	}
}

func TestCreateNodeHonorsCallerCancellationAndCleansUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var commands [][]string
	created := false
	runner := vmRunnerFunc(func(commandContext context.Context, command runtimes.Command) ([]byte, error) {
		commands = append(commands, command.Args)
		switch command.Args[0] {
		case "create":
			created = true
		case "list":
			if created {
				return json.Marshal(map[string]any{"name": "synthetic-canceled", "status": "Stopped", "vmType": "qemu"})
			}
		case "start":
			deadline, ok := commandContext.Deadline()
			if !ok || time.Until(deadline) > time.Second {
				t.Error("first-start budget overrode the caller's shorter deadline")
			}
			cancel()
			return []byte("synthetic image preparation canceled"), commandContext.Err()
		case "delete":
			if commandContext.Err() != nil {
				t.Errorf("cleanup inherited the canceled caller context: %v", commandContext.Err())
			}
			created = false
		default:
			t.Errorf("unexpected command after canceled start: %v", command.Args)
		}
		return nil, nil
	})
	manager := NewWithStateDir(runner, t.TempDir())

	_, err := manager.CreateNode(ctx, CreateRequest{
		Name: "synthetic-canceled", Owner: "synthetic-cluster", Image: "ubuntu-24.04",
		CPUs: 2, MemoryMiB: 2048, DiskGiB: 20, Network: "user-v2", Start: true,
		Provision: "printf synthetic-provision",
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "synthetic image preparation canceled") {
		t.Fatalf("caller cancellation lost its cause or diagnostic: %v", err)
	}
	if created || len(commands) != 5 || !slices.Equal(commands[4], []string{"delete", "--force", "synthetic-canceled"}) {
		t.Errorf("canceled create did not clean only its newly created instance: %v", commands)
	}
	if _, err := os.Stat(manager.metadataPath("synthetic-canceled")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("canceled create left ownership metadata: %v", err)
	}
}

func TestMachineProbesUseGuestRootWithoutChangingExecWorkdir(t *testing.T) {
	var commands [][]string
	runner := vmRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		commands = append(commands, command.Args)
		if slices.Contains(command.Args, vmResourceScript) {
			return []byte("42 1048576\n"), nil
		}
		return nil, nil
	})
	manager := New(runner)

	if err := manager.waitForSSH(context.Background(), "synthetic-probe", time.Second); err != nil {
		t.Fatalf("SSH readiness probe: %v", err)
	}
	if _, err := manager.ResourceStats(context.Background(), "synthetic-probe"); err != nil {
		t.Fatalf("resource probe: %v", err)
	}
	if _, err := manager.Exec(context.Background(), "synthetic-probe", []string{"pwd"}, nil); err != nil {
		t.Fatalf("user command: %v", err)
	}
	want := [][]string{
		{"shell", "--workdir=/", "synthetic-probe", "--", "true"},
		{"shell", "--workdir=/", "synthetic-probe", "--", "sh", "-c", vmResourceScript},
		{"shell", "synthetic-probe", "--", "pwd"},
	}
	if len(commands) != len(want) {
		t.Fatalf("command count = %d, want %d", len(commands), len(want))
	}
	for index := range want {
		if !slices.Equal(commands[index], want[index]) {
			t.Errorf("command %d = %v, want %v", index, commands[index], want[index])
		}
	}
}
