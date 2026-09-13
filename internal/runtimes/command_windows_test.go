//go:build windows

package runtimes

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

func TestExecRunnerCommandsDoNotOpenWindowsConsoles(t *testing.T) {
	command := newExecCommand(context.Background(), Command{
		Name: "cmd.exe",
		Args: []string{"/C", "exit 0"},
	})
	if command.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	if !command.SysProcAttr.HideWindow {
		t.Fatal("HideWindow is false")
	}
	if command.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW", command.SysProcAttr.CreationFlags)
	}
}
