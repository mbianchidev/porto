package runtimefiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func TestDirectoryRequestValidatesIdentityAndReadOnly(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	resource := datafiles.Resource{Kind: "volume", Name: "fixture", ID: "volume-1"}
	descriptor := Descriptor{Resource: resource, RootPath: directory}
	request := datafiles.Request{Action: "list", Path: ".", Identity: "stale"}
	var output bytes.Buffer
	if err := ExecuteDirectory(context.Background(), descriptor, request, bytes.NewReader(nil), &output); !errors.Is(err, datafiles.ErrConflict) {
		t.Fatalf("stale identity error=%v", err)
	}
	request.Identity = resource.Fingerprint()
	if err := ExecuteDirectory(context.Background(), descriptor, request, bytes.NewReader(nil), &output); err != nil {
		t.Fatal(err)
	}
	var listing datafiles.Listing
	if err := json.Unmarshal(output.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Path != "fixture.txt" {
		t.Fatalf("unexpected listing: %+v", listing)
	}
	descriptor.Resource.ReadOnly = true
	request.Identity = descriptor.Resource.Fingerprint()
	request.Action, request.Path = "write", "new.txt"
	if err := ExecuteDirectory(context.Background(), descriptor, request, bytes.NewBufferString("new"), &bytes.Buffer{}); !errors.Is(err, datafiles.ErrUnsupported) {
		t.Fatalf("read-only filesystem accepted write: %v", err)
	}
}
