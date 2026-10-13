package runtimefiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestContainerTextEditsEnforcePreviewLimits(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		content string
		wantErr error
	}{
		{"at byte limit", strings.Repeat("x", datafiles.MaxTextBytes), nil},
		{"over byte limit", strings.Repeat("x", datafiles.MaxTextBytes+1), datafiles.ErrLimit},
		{"UTF-8 byte limit", strings.Repeat("\u754c", datafiles.MaxTextBytes/3+1), datafiles.ErrLimit},
		{"binary edit", "synthetic\x00binary", datafiles.ErrUnsupported},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(filepath.Join(directory, "fixture.txt"), []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			preview, err := datafiles.Read(context.Background(), directory, "fixture.txt", false)
			if err != nil {
				t.Fatal(err)
			}
			resource := datafiles.Resource{Kind: "container", Name: "fixture", ID: "synthetic-container"}
			request := datafiles.Request{
				Action: "write", Path: "fixture.txt", Identity: resource.Fingerprint(), SHA256: preview.SHA256,
			}
			err = ExecuteDirectory(context.Background(), Descriptor{Resource: resource, RootPath: directory},
				request, strings.NewReader(fixture.content), &bytes.Buffer{})
			if !errors.Is(err, fixture.wantErr) {
				t.Fatalf("edit error = %v, want %v", err, fixture.wantErr)
			}
			actual, readErr := os.ReadFile(filepath.Join(directory, "fixture.txt"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			expected := fixture.content
			if fixture.wantErr != nil {
				expected = "original"
			}
			if string(actual) != expected {
				t.Fatal("edit did not preserve the expected contents")
			}
		})
	}
}

func TestContainerFilesystemFixtures(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		pid      uint32
		readOnly bool
	}{
		{"running", 42, false},
		{"stopped", 0, false},
		{"read-only", 42, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Mkdir(filepath.Join(directory, "mounted"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "mounted", "fixture.txt"), []byte("synthetic"), 0o600); err != nil {
				t.Fatal(err)
			}
			resource := datafiles.Resource{Kind: "container", Name: "fixture", ID: "synthetic", ReadOnly: fixture.readOnly}
			descriptor := Descriptor{
				Resource: resource, RootPath: directory, PID: fixture.pid,
				Display: []datafiles.Mount{{Path: "/mounted", Kind: "volume", ReadOnly: true}},
			}
			request := datafiles.Request{Action: "list", Path: ".", Identity: resource.Fingerprint()}
			var output bytes.Buffer
			if err := ExecuteDirectory(context.Background(), descriptor, request, nil, &output); err != nil {
				t.Fatal(err)
			}
			var listing datafiles.Listing
			if err := json.Unmarshal(output.Bytes(), &listing); err != nil {
				t.Fatal(err)
			}
			if listing.ReadOnly != fixture.readOnly || len(listing.Mounts) != 1 || len(listing.Entries) != 1 {
				t.Fatalf("unexpected container capability: %+v", listing)
			}
			request.Action, request.Path = "read", "mounted/fixture.txt"
			output.Reset()
			if err := ExecuteDirectory(context.Background(), descriptor, request, nil, &output); err != nil {
				t.Fatal(err)
			}
			var content datafiles.Content
			if err := json.Unmarshal(output.Bytes(), &content); err != nil || !content.ReadOnly {
				t.Fatalf("mount preview did not preserve read-only capability: %+v, %v", content, err)
			}
			request.Action, request.Confirm, request.SHA256 = "delete", true, content.SHA256
			if err := ExecuteDirectory(context.Background(), descriptor, request, nil, &bytes.Buffer{}); !errors.Is(err, datafiles.ErrUnsupported) {
				t.Fatalf("read-only mount accepted deletion: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			request.Action, request.Path = "list", "."
			if err := ExecuteDirectory(ctx, descriptor, request, nil, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("filesystem request did not preserve cancellation: %v", err)
			}
		})
	}
}
