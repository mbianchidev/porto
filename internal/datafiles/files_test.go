package datafiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileEditingRejectsConcurrentChange(t *testing.T) {
	directory := t.TempDir()
	file := filepath.Join(directory, "fixture.txt")
	if err := os.WriteFile(file, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	original, err := Read(context.Background(), directory, "fixture.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("external writer"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := Write(context.Background(), directory, "fixture.txt", []byte("edited"), original.SHA256); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected concurrent-write conflict, got %v", err)
	}
	after, err := os.ReadFile(file)
	if err != nil || string(after) != "external writer" {
		t.Fatal("external write was overwritten")
	}
}
