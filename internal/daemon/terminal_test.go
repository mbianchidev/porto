package daemon

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/mbianchidev/porto/internal/kubernetes"
)

func TestAllowedShell(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "ash", "/bin/sh", "/bin/bash", "/bin/ash"} {
		if !allowedShell(shell) {
			t.Errorf("expected %q to be allowed", shell)
		}
	}
	for _, shell := range []string{"zsh", "sh -c id", "../../bin/sh", ""} {
		if allowedShell(shell) {
			t.Errorf("expected %q to be rejected", shell)
		}
	}
}

func TestK9sTerminalCommandScopesManagedCluster(t *testing.T) {
	command := k9sTerminalCommand(context.Background(), kubernetes.Cluster{
		Context:        "porto-dev",
		KubeconfigPath: "/tmp/dev.yaml",
	})
	want := []string{
		"k9s",
		"--kubeconfig", "/tmp/dev.yaml",
		"--context", "porto-dev",
		"--all-namespaces",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	if !slices.Contains(command.Env, "KUBECONFIG=/tmp/dev.yaml") {
		t.Fatalf("command environment does not contain managed kubeconfig: %q", command.Env)
	}
	if !slices.Contains(command.Env, "TERM=xterm-256color") || !slices.Contains(command.Env, "COLORTERM=truecolor") {
		t.Fatalf("command environment does not contain terminal capabilities: %q", command.Env)
	}
}

func TestVMTerminalCommandUsesInteractiveLimaShell(t *testing.T) {
	command := vmTerminalCommand(context.Background(), "test-vm")
	want := []string{
		"limactl", "shell", "--tty=true", "test-vm", "--",
		"sh", "-lc", `cd "$HOME" && exec env PS1="$1 $ " sh -i`, "porto-shell", "test-vm",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
	if !slices.Contains(command.Env, "TERM=xterm-256color") || !slices.Contains(command.Env, "COLORTERM=truecolor") {
		t.Fatalf("command environment does not contain terminal capabilities: %q", command.Env)
	}
}

func TestPodTerminalCommandUsesKubectlExec(t *testing.T) {
	command := podTerminalCommand(context.Background(), []string{
		"--context", "porto-dev",
		"exec", "--stdin", "--tty", "--namespace", "default", "api",
		"--container", "app", "--", "sh",
	})
	want := []string{
		"kubectl",
		"--context", "porto-dev",
		"exec", "--stdin", "--tty", "--namespace", "default", "api",
		"--container", "app", "--", "sh",
	}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
}
