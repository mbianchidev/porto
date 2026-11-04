package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestReadCNINetworkIdentitiesUsesFreshSelectedNamespaceMetadata(t *testing.T) {
	root := t.TempDir()
	namespace := filepath.Join(root, "synthetic")
	if err := os.MkdirAll(namespace, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(directory, name, id string) {
		t.Helper()
		document, err := json.Marshal(map[string]any{
			"name": name, "nerdctlID": id, "plugins": []any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name+".conflist"), document, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	write(namespace, "synthetic-first", first)
	write(namespace, "synthetic-second", second)
	write(root, "synthetic-first", strings.Repeat("c", 64))
	if err := os.WriteFile(filepath.Join(namespace, "unrelated.conflist"), []byte(`{"name":"unrelated","plugins":[],"nerdctlID":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	directories := []string{namespace, root}
	got, err := readCNINetworkIdentities(directories, []string{"synthetic-first", "synthetic-second"})
	want := []cniNetworkIdentity{{Name: "synthetic-first", ID: first}, {Name: "synthetic-second", ID: second}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("selected namespace identities = %+v, %v; want %+v", got, err, want)
	}
	replacement := strings.Repeat("d", 64)
	write(namespace, "synthetic-first", replacement)
	got, err = readCNINetworkIdentities(directories, []string{"synthetic-first"})
	if err != nil || len(got) != 1 || got[0].ID != replacement {
		t.Fatalf("network recreation returned a stale identity: %+v, %v", got, err)
	}
	write(namespace, "synthetic-foreign", "")
	got, err = readCNINetworkIdentities(directories, []string{"synthetic-foreign"})
	if err != nil || len(got) != 1 || got[0].ID != "" {
		t.Fatalf("a non-nerdctl network was assigned an invented identity: %+v, %v", got, err)
	}
}

func TestReadCNINetworkIdentitiesRejectsMissingOrInvalidMetadata(t *testing.T) {
	for _, test := range []struct {
		name, document, message string
		networks                []string
	}{
		{name: "missing arguments", message: "at least one"},
		{name: "invalid name", networks: []string{"synthetic\nnetwork"}, message: "valid CNI network"},
		{name: "missing network", networks: []string{"synthetic"}, message: "no network found matching:"},
		{name: "invalid JSON", document: "{", networks: []string{"synthetic"}, message: "decode CNI configuration"},
		{name: "invalid ID type", document: `{"name":"synthetic","nerdctlID":42}`, networks: []string{"synthetic"}, message: "decode CNI network identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if test.document != "" {
				if err := os.WriteFile(filepath.Join(directory, "synthetic.conflist"), []byte(test.document), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			identities, err := readCNINetworkIdentities([]string{directory}, test.networks)
			if err == nil || !strings.Contains(err.Error(), test.message) || identities != nil {
				t.Fatalf("invalid metadata returned partial/successful identities: %+v, %v", identities, err)
			}
		})
	}
}

func TestReadCNINetworkIdentitiesDoesNotHideBrokenNamespaceConfiguration(t *testing.T) {
	root := t.TempDir()
	namespace := filepath.Join(root, "synthetic")
	if err := os.MkdirAll(namespace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(namespace, "synthetic.conflist"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "synthetic.conflist"), []byte(`{"name":"synthetic","nerdctlID":"unrelated-root-network"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	identities, err := readCNINetworkIdentities([]string{namespace, root}, []string{"synthetic"})
	if err == nil || !strings.Contains(err.Error(), "decode CNI configuration") || identities != nil {
		t.Fatalf("invalid namespaced metadata fell back to another network identity: %+v, %v", identities, err)
	}
}
