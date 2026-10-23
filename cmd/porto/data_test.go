package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

type dataTestTransport func(*http.Request) (*http.Response, error)

func (f dataTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func mockDataAPI(t *testing.T, handler func(*http.Request) (any, error)) {
	t.Helper()
	original := http.DefaultTransport
	http.DefaultTransport = dataTestTransport(func(request *http.Request) (*http.Response, error) {
		var result any
		var err error
		if request.URL.Path == "/api/health" {
			result = map[string]any{"status": "ok", "apiVersion": config.APIVersion}
		} else {
			result, err = handler(request)
		}
		if err != nil {
			return nil, err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(string(body))),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
}

func TestStorageCLIConfirmsOnlyFrozenPreviewSelections(t *testing.T) {
	selection := dataops.Selection{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	submitted := false
	mockDataAPI(t, func(request *http.Request) (any, error) {
		switch request.URL.Path {
		case "/api/docker/storage/preview":
			return portodocker.PrunePreview{
				Request:    dataops.Request{Action: "prune", Selections: []dataops.Selection{selection}},
				Candidates: []portodocker.StorageResource{{Resource: datafiles.Resource{Kind: "volume", Name: selection.Name, ID: selection.ID}}},
				Token:      "synthetic-preview",
			}, nil
		case "/api/data/operations":
			var body dataops.Request
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if len(body.Categories) != 0 || len(body.Selections) != 1 || body.Selections[0] != selection ||
				!body.Confirm || body.Preview != "synthetic-preview" {
				return nil, fmt.Errorf("CLI widened the confirmed preview: %+v", body)
			}
			submitted = true
			return dataops.Operation{ID: 1}, nil
		}
		return nil, fmt.Errorf("unexpected data API path: %s", request.URL.Path)
	})
	if err := dockerDataCmd([]string{"storage", "prune", "--category", "volume", "--confirm"}); err != nil {
		t.Fatal(err)
	}
	if !submitted {
		t.Fatal("confirmed preview was not submitted")
	}
}

func TestStorageCLIResolvesExactObjectSelectors(t *testing.T) {
	mockDataAPI(t, func(request *http.Request) (any, error) {
		if request.URL.Path == "/api/docker/storage" {
			return portodocker.StorageUsage{Resources: []portodocker.StorageResource{{
				Resource: datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"},
			}}}, nil
		}
		if request.URL.Path == "/api/docker/storage/preview" {
			var body dataops.Request
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if len(body.Selections) != 1 || body.Selections[0].ID != "volume-1" || len(body.Categories) != 0 {
				return nil, fmt.Errorf("selector did not resolve its exact inventory identity: %+v", body)
			}
			return portodocker.PrunePreview{Request: body, Token: "synthetic"}, nil
		}
		return nil, fmt.Errorf("unexpected data API path: %s", request.URL.Path)
	})
	if err := dockerDataCmd([]string{"storage", "prune", "--objects", "volume:synthetic-volume"}); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeExportRejectsExistingLocalDestinationBeforeStartingJob(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "existing.tar")
	if err := os.WriteFile(destination, []byte("original local archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	mockDataAPI(t, func(request *http.Request) (any, error) {
		t.Errorf("an existing download destination reached the daemon: %s", request.URL.Path)
		return nil, errors.New("unexpected request")
	})
	err := dockerVolumeDataCmd([]string{"export", "synthetic-volume", destination, "--confirm"})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("local conflict was not rejected before export: %v", err)
	}
}

func TestVolumeDownloadPinsThePreviewedArchiveChecksum(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "synthetic.txt"), []byte("synthetic archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if _, err := datafiles.Export(context.Background(), &archive, source, datafiles.Resource{Kind: "volume", ID: "volume-1"}); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = dataTestTransport(func(request *http.Request) (*http.Response, error) {
		body := archive.Bytes()
		contentType := "application/x-tar"
		if !strings.HasSuffix(request.URL.Path, "/archive") {
			var err error
			body, err = json.Marshal(dataops.Operation{
				ID: 12, Status: "succeeded", Result: dataops.Result{Archive: &dataops.Archive{
					SHA256: strings.Repeat("a", 64), Bytes: int64(archive.Len()),
				}},
			})
			if err != nil {
				return nil, err
			}
			contentType = "application/json"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	directory := t.TempDir()
	destination := filepath.Join(directory, "download.tar")
	err := downloadCompletedVolumeExport(12, destination)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a different valid archive was published: %v", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("unverified download or temporary file remained: %+v, %v", entries, err)
	}
}

func TestNativeFilesCLIReportsTheExactRequestedResourceCapability(t *testing.T) {
	mockDataAPI(t, func(request *http.Request) (any, error) {
		query := request.URL.Query()
		if request.URL.Path != "/api/files/capabilities" || query.Get("kind") != "container" ||
			query.Get("name") != "synthetic-container" || query.Get("writable") != "true" {
			return nil, fmt.Errorf("native capability lost the resource or write boundary: %s", request.URL)
		}
		return map[string]any{"supported": false, "message": "Synthetic stopped resource is unavailable."}, nil
	})
	if err := dockerFilesCmd([]string{"container", "synthetic-container", "--writable"}); err != nil {
		t.Fatal(err)
	}
}
