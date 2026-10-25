package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeWindowsInternalDispatchPrecedesUserConfiguration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("non-Windows no-mutation gate")
	}
	root := t.TempDir()
	home := filepath.Join(root, "must-not-be-created")
	t.Setenv("PORTO_HOME", home)
	for _, args := range [][]string{
		{"windows-containers-worker", "S-1-5-21-111-222-333-1001"},
		{"windows-containers-elevated", "install", "S-1-5-21-111-222-333-1001", "source", "bundle"},
		{"docker", "windows", "install"},
	} {
		err := runCommand(args)
		if err == nil || !strings.Contains(err.Error(), "Windows") {
			t.Fatalf("native platform gate %v = %v", args, err)
		}
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("internal native dispatch initialized ordinary user configuration: %v", err)
		}
	}
}
