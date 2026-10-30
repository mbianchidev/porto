package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestKubeconfigStorageMovesExistingStateAndResetsToDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	original, err := KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"synthetic.yaml": "synthetic kubeconfig credentials",
		"synthetic.json": `{"name":"synthetic","provider":"kind"}`,
		filepath.Join("autoscaling", "synthetic", "settings.json"): `{"enabled":false}`,
	}
	for name, contents := range files {
		target := filepath.Join(original, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	destination := filepath.Join(t.TempDir(), "synthetic kubeconfigs \u03b4")
	for _, selection := range []string{destination, ""} {
		applied, err := ApplyKubeconfigDirectory(context.Background(), selection)
		if err != nil {
			t.Fatal(err)
		}
		active, err := KubernetesConfigDir()
		if err != nil || active != applied {
			t.Fatalf("active storage = %q, %v; want %q", active, err, applied)
		}
		for name, contents := range files {
			path := filepath.Join(active, name)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatalf("moved %s = %q, %v", name, data, err)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private kubeconfig mode: %v, %v", info, err)
				}
			}
		}
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous storage was not removed: %v", err)
	}
}

func TestKubeconfigStorageRejectsCollisionsAndOverlappingFolders(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	source, err := KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	sentinel := filepath.Join(destination, "unrelated")
	if err := os.WriteFile(sentinel, []byte("synthetic unrelated file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{destination, filepath.Join(source, "nested"), filepath.Dir(source)} {
		if err := ValidateKubeconfigDirectoryChange(path); err == nil {
			t.Fatalf("unsafe destination was accepted: %s", path)
		}
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "synthetic unrelated file" {
		t.Fatalf("unrelated destination changed: %q, %v", data, err)
	}
}

func TestKubeconfigStorageRollsBackPartialCopyAndPreservesSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires an explicitly enabled Windows developer mode")
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	source, err := KubernetesConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.yaml"), []byte("synthetic retained credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "external"), filepath.Join(source, "z-link")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination")
	if _, err := ApplyKubeconfigDirectory(context.Background(), destination); err == nil {
		t.Fatal("migration followed a symlink")
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial destination remains: %v", err)
	}
	if active, err := KubernetesConfigDir(); err != nil || active != source {
		t.Fatalf("failed migration changed active storage: %q, %v", active, err)
	}
	if data, err := os.ReadFile(filepath.Join(source, "a.yaml")); err != nil || string(data) != "synthetic retained credentials" {
		t.Fatalf("source was not preserved: %q, %v", data, err)
	}
}

func TestKubeconfigStorageCancellationAndInvalidConfigurationStayVisible(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ApplyKubeconfigDirectory(ctx, filepath.Join(t.TempDir(), "canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("migration cancellation = %v", err)
	}
	for _, value := range []string{"relative/path", "/synthetic\ninvalid", "/synthetic\x00invalid"} {
		if _, err := NormalizeKubeconfigDirectory(value); err == nil {
			t.Fatalf("invalid path accepted: %q", value)
		}
	}
	if err := os.WriteFile(filepath.Join(root, kubeconfigLocationFile), []byte(`{"directory":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := KubernetesConfigDir(); err == nil {
		t.Fatal("invalid active storage silently fell back to the default")
	}
}

func TestKubeconfigDestinationValidationUsesOnlyTheSelectedDirectory(t *testing.T) {
	empty := t.TempDir()
	if err := validateKubeconfigDestination(empty); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "not-created", "kubeconfigs")
	if err := validateKubeconfigDestination(missing); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "synthetic-file")
	if err := os.WriteFile(file, []byte("synthetic private data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateKubeconfigDestination(file); err == nil {
		t.Fatal("a regular file was accepted as storage")
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(t.TempDir(), "outside-link")
		if err := os.Symlink(empty, link); err != nil {
			t.Fatal(err)
		}
		if err := validateKubeconfigDestination(link); err == nil {
			t.Fatal("destination validation followed a directory symlink")
		}
		if err := validateKubeconfigDestination(string(filepath.Separator)); err == nil {
			t.Fatal("a filesystem root was accepted as dedicated storage")
		}
	}
}
