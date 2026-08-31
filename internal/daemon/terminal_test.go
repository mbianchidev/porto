package daemon

import (
	"context"
	"reflect"
	"testing"
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

func TestVMTerminalCommandUsesInteractiveLimaShell(t *testing.T) {
	command := vmTerminalCommand(context.Background(), "test-vm")
	want := []string{"limactl", "shell", "--tty=true", "test-vm"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command args = %q, want %q", command.Args, want)
	}
}
