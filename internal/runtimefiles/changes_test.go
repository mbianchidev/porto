package runtimefiles

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCompareDirectoriesReportsAddedModifiedAndDeletedPaths(t *testing.T) {
	lower, upper := t.TempDir(), t.TempDir()
	for _, item := range []struct{ root, name, text string }{
		{lower, "removed", "synthetic old"}, {lower, "changed", "old"},
		{upper, "changed", "synthetic new"}, {upper, "added", "synthetic addition"},
	} {
		if err := os.WriteFile(filepath.Join(item.root, item.name), []byte(item.text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := CompareDirectories(context.Background(), lower, upper, &output); err != nil {
		t.Fatal(err)
	}
	var changes []FilesystemChange
	if err := json.Unmarshal(output.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	want := []FilesystemChange{{Path: "/added", Kind: 1}, {Path: "/changed", Kind: 0}, {Path: "/removed", Kind: 2}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
}
