package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTryAcquireExistingDoesNotCreateReplaceOrTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.lock")
	if _, err := TryAcquireExisting(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing writer lock was created: %v", err)
	}
	if err := os.WriteFile(path, []byte("synthetic writer evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := TryAcquireExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := TryAcquireExisting(path); err == nil {
		_ = second.Close()
		t.Fatal("nonblocking acquisition ignored the current writer")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("writer-lock inode changed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "synthetic writer evidence" {
		t.Fatal("writer lock was truncated")
	}
}
