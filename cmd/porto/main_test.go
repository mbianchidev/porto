package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseLogArgsAllowsOptionsBeforeAndAfterProject(t *testing.T) {
	for _, args := range [][]string{
		{"--stream", "stderr", "-n", "50", "app"},
		{"app", "--stream=stderr", "-n=50"},
	} {
		project, stream, limit, clear, err := parseLogArgs(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if project != "app" || stream != "stderr" || limit != 50 || clear {
			t.Fatalf("parse %v = %q, %q, %d, %t", args, project, stream, limit, clear)
		}
	}
}

func TestParseLogArgsClear(t *testing.T) {
	project, stream, _, clear, err := parseLogArgs([]string{"app", "--clear", "--stream", "stdout"})
	if err != nil {
		t.Fatalf("parse clear: %v", err)
	}
	if project != "app" || stream != "stdout" || !clear {
		t.Fatalf("clear args = %q, %q, %t", project, stream, clear)
	}
}

func TestBundledRuntimePathPrependsExistingDirectories(t *testing.T) {
	base := t.TempDir()
	executable := filepath.Join(base, "porto")
	for _, directory := range []string{
		filepath.Join(base, "runtime", "bin"),
		filepath.Join(base, "runtime", "lima", "bin"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	current := filepath.Join(base, "system-bin")
	got := filepath.SplitList(bundledRuntimePath(executable, current))
	if len(got) != 3 || got[0] != filepath.Join(base, "runtime", "bin") || got[1] != filepath.Join(base, "runtime", "lima", "bin") || got[2] != current {
		t.Fatalf("bundled runtime PATH = %q", strings.Join(got, "|"))
	}
}

func TestBundledRuntimePathDoesNotDuplicateEntries(t *testing.T) {
	base := t.TempDir()
	executable := filepath.Join(base, "porto")
	runtimeBin := filepath.Join(base, "runtime", "bin")
	if err := os.MkdirAll(runtimeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	current := strings.Join([]string{runtimeBin, filepath.Join(base, "system-bin")}, string(os.PathListSeparator))
	if got := bundledRuntimePath(executable, current); got != current {
		t.Fatalf("bundled runtime PATH duplicated an existing entry: %q", got)
	}
}

func TestBundledRuntimePathResolvesExecutableSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional privileges on Windows")
	}
	base := t.TempDir()
	executable := filepath.Join(base, "app", "porto")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("porto"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeBin := filepath.Join(base, "app", "runtime", "bin")
	if err := os.MkdirAll(runtimeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "bin", "porto")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	resolvedRuntimeBin, err := filepath.EvalSymlinks(runtimeBin)
	if err != nil {
		t.Fatal(err)
	}
	got := filepath.SplitList(bundledRuntimePath(link, ""))
	if len(got) != 1 || got[0] != resolvedRuntimeBin {
		t.Fatalf("bundled runtime PATH = %q, want %q", got, resolvedRuntimeBin)
	}
}
