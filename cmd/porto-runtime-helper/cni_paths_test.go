package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCNIConfigDirectoriesPreferRootlessNamespace(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CNI_NET_DIR", "")
	t.Setenv("CONTAINERD_NAMESPACE", "porto")

	directories := cniConfigDirectories()
	want := filepath.Join(home, ".config", "cni", "net.d", "porto")
	if len(directories) < 2 || directories[0] != want {
		t.Fatalf("CNI directories = %q, want namespace directory first", directories)
	}
}

func TestFindCNIConfigUsesNamespaceDirectory(t *testing.T) {
	root := t.TempDir()
	namespaceDirectory := filepath.Join(root, "default")
	if err := os.MkdirAll(namespaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(namespaceDirectory, "nerdctl-backend.conflist")
	if err := os.WriteFile(path, []byte(`{"name":"backend","plugins":[{"type":"bridge"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	found, list, err := findCNIConfig([]string{namespaceDirectory, root}, "backend")
	if err != nil {
		t.Fatal(err)
	}
	if found != path || !list {
		t.Fatalf("CNI config = %q, list=%t; want %q, true", found, list, path)
	}
	if !hasCNIConfig([]string{namespaceDirectory, root}) {
		t.Fatal("CNI configuration was not detected")
	}
}
