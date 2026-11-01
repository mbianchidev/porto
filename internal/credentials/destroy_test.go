package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDestroyOwnedEncryptedStateRemovesOnlyItsKeyAndRejectsRetainedHandles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "synthetic-owner")
	backend := &memoryStore{values: map[string]string{"unrelated/account": "synthetic-unrelated-secret"}}
	files := NewEncryptedFiles(root, "synthetic-owner-service", backend)
	if err := files.Set("owned", []byte("synthetic-owned-secret")); err != nil {
		t.Fatal(err)
	}
	if err := files.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "owned.sealed")); !os.IsNotExist(err) {
		t.Fatalf("owned ciphertext retained: %v", err)
	}
	if _, err := files.Get("owned"); err == nil {
		t.Fatal("destroyed credential handle remained usable")
	}
	if err := files.Set("new", []byte("synthetic")); err == nil {
		t.Fatal("retained handle recreated a removed key")
	}
	if len(backend.values) != 1 || backend.values["unrelated/account"] != "synthetic-unrelated-secret" {
		t.Fatal("cleanup removed unrelated credentials or retained owned key")
	}
}
