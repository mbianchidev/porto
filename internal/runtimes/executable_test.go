package runtimes

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLookPathInDirectoriesFindsExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable mode bits are not meaningful on Windows")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	found, err := lookPathInDirectories("provider", []string{t.TempDir(), directory})
	if err != nil {
		t.Fatalf("find executable: %v", err)
	}
	if found != path {
		t.Fatalf("found path = %q, want %q", found, path)
	}
}

func TestLookPathInDirectoriesRejectsNonExecutableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable mode bits are not meaningful on Windows")
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "provider"), []byte("not executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lookPathInDirectories("provider", []string{directory}); err == nil {
		t.Fatal("non-executable file was accepted")
	}
}

func TestResolveQEMUPathPrefersConfiguredOverride(t *testing.T) {
	const override = "/Applications/QEMU/bin/qemu-system-aarch64"
	path, err := ResolveQEMUPath(
		"qemu-system-aarch64",
		func(name string) (string, error) {
			if name == override {
				return name, nil
			}
			return "", os.ErrNotExist
		},
		func(name string) string {
			if name == "QEMU_SYSTEM_AARCH64" {
				return override
			}
			return ""
		},
	)
	if err != nil {
		t.Fatalf("resolve QEMU override: %v", err)
	}
	if path != override {
		t.Fatalf("resolved path = %q, want %q", path, override)
	}
}

func TestLookPathFindsHomebrewExecutableOutsidePATH(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Homebrew fallback is macOS-specific")
	}
	const path = "/opt/homebrew/bin/qemu-system-aarch64"
	if _, err := os.Stat(path); err != nil {
		t.Skip("Homebrew QEMU is not installed")
	}
	t.Setenv("PATH", "/usr/bin:/bin")
	found, err := LookPath("qemu-system-aarch64")
	if err != nil {
		t.Fatalf("find Homebrew QEMU: %v", err)
	}
	if found != path {
		t.Fatalf("found path = %q, want %q", found, path)
	}
}
