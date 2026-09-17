package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersionMatchesPackageMetadata(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate version test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))

	for _, name := range []string{"ui/package.json", "ui/electron/package.json"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if manifest.Version != Version {
			t.Fatalf("%s version = %q, want %q", name, manifest.Version, Version)
		}
	}
}
