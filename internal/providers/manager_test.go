package providers

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type providerRunner struct {
	mu       sync.Mutex
	commands []runtimes.Command
}

func (r *providerRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	return []byte("QEMU emulator version 11.1.1"), nil
}

func TestInstallRejectsUnknownProvider(t *testing.T) {
	_, err := New(nil).Install(context.Background(), "unknown")
	if err == nil || !strings.Contains(err.Error(), "unknown runtime provider") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestK0sProviderUsesLima(t *testing.T) {
	provider, ok := findTool("k0s")
	if !ok {
		t.Fatal("k0s provider is missing")
	}
	if provider.command != "limactl" || provider.formula != "lima" {
		t.Fatalf("unexpected k0s provider: %+v", provider)
	}
}

func TestK9sProviderUsesNativeBinary(t *testing.T) {
	provider, ok := findTool("k9s")
	if !ok {
		t.Fatal("k9s provider is missing")
	}
	if provider.command != "k9s" || provider.formula != "k9s" {
		t.Fatalf("unexpected k9s provider: %+v", provider)
	}
}

func TestQEMUProviderUsesArchitectureBinary(t *testing.T) {
	provider, ok := findTool("qemu")
	if !ok {
		t.Fatal("qemu provider is missing")
	}
	if !strings.HasPrefix(provider.command, "qemu-system-") || provider.formula != "qemu" {
		t.Fatalf("unexpected qemu provider: %+v", provider)
	}
}

func TestQEMUInstallReturnsMacInstructions(t *testing.T) {
	manager := New(nil)
	manager.goos = "darwin"
	manager.getenv = func(string) string { return "" }
	manager.lookPath = func(string) (string, error) {
		return "", errors.New("not found")
	}
	status, err := manager.Install(context.Background(), "qemu")
	if err == nil ||
		!strings.Contains(err.Error(), "brew install qemu") ||
		!strings.Contains(err.Error(), "restart Porto") {
		t.Fatalf("unexpected QEMU install guidance: status=%+v error=%v", status, err)
	}
	if status.Installed {
		t.Fatalf("missing QEMU was reported installed: %+v", status)
	}
}

func TestQEMUStatusUsesDynamicallyDiscoveredPath(t *testing.T) {
	runner := &providerRunner{}
	manager := New(runner)
	manager.getenv = func(string) string { return "" }
	manager.lookPath = func(name string) (string, error) {
		if name == "qemu-system-aarch64" || name == "qemu-system-x86_64" {
			return "/opt/homebrew/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	statuses := manager.Status(context.Background())
	var qemu Status
	for _, status := range statuses {
		if status.Name == "qemu" {
			qemu = status
			break
		}
	}
	if !qemu.Installed || !strings.Contains(qemu.Version, "11.1.1") {
		t.Fatalf("QEMU was not detected: %+v", qemu)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 || !strings.HasPrefix(runner.commands[0].Name, "/opt/homebrew/bin/qemu-system-") {
		t.Fatalf("QEMU was not executed through its discovered path: %+v", runner.commands)
	}
}

func TestQEMUStatusHonorsLimaOverride(t *testing.T) {
	runner := &providerRunner{}
	manager := New(runner)
	manager.getenv = func(name string) string {
		if name == "QEMU_SYSTEM_AARCH64" || name == "QEMU_SYSTEM_X86_64" {
			return "/Applications/QEMU/bin/" + qemuCommand()
		}
		return ""
	}
	manager.lookPath = func(name string) (string, error) {
		if strings.HasPrefix(name, "/Applications/QEMU/bin/qemu-system-") {
			return name, nil
		}
		return "", errors.New("not found")
	}
	statuses := manager.Status(context.Background())
	for _, status := range statuses {
		if status.Name == "qemu" && status.Installed {
			return
		}
	}
	t.Fatalf("QEMU override was not detected: %+v", statuses)
}

func TestInstallUsesDynamicallyDiscoveredBrew(t *testing.T) {
	runner := &providerRunner{}
	manager := New(runner)
	manager.goos = "darwin"
	manager.getenv = func(string) string { return "" }
	manager.lookPath = func(name string) (string, error) {
		if name == "brew" {
			return "/opt/homebrew/bin/brew", nil
		}
		return "", errors.New("not found")
	}
	if _, err := manager.Install(context.Background(), "lima"); err == nil {
		t.Fatal("expected unavailable provider after simulated brew install")
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.commands) != 1 || runner.commands[0].Name != "/opt/homebrew/bin/brew" {
		t.Fatalf("brew did not use its discovered path: %+v", runner.commands)
	}
}
