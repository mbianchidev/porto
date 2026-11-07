package etcqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func syntheticSnapshot() ([]byte, SnapshotStatus) {
	payload := []byte("SQLite format 3\x00synthetic native recovery fixture")
	sum := sha256.Sum256(payload)
	image := append(append([]byte(nil), payload...), sum[:]...)
	return image, SnapshotStatus{
		Format: 1, SchemaVersion: 2, ClusterID: 123, MemberID: 7,
		Revision: 4, CompactRevision: -1, ConfigurationID: 1, Bytes: uint64(len(image)), SHA256: hex.EncodeToString(sum[:]),
	}
}

func TestNativeDiscoveryPinsIdentityAndCapability(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		change    func(*Discovery, http.Header)
		wantError string
	}{
		{name: "valid"},
		{name: "foreign cluster", change: func(d *Discovery, _ http.Header) { d.ClusterID = "124" }, wantError: "identity"},
		{name: "foreign member", change: func(d *Discovery, _ http.Header) { d.MemberID = "8" }, wantError: "identity"},
		{name: "missing capability", change: func(d *Discovery, _ http.Header) { d.Capabilities = nil }, wantError: "native-snapshot-v1"},
		{name: "projection is not backup", change: func(d *Discovery, _ http.Header) { d.SnapshotPath = "/etcqlite/v1/projections" }, wantError: "snapshot path"},
		{name: "unbound ETag", change: func(_ *Discovery, h http.Header) { h.Set("ETag", `"other"`) }, wantError: "ETag"},
		{name: "wrong kind", change: func(d *Discovery, _ http.Header) { d.Kind = "sqlite" }, wantError: "discovery format"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/etcqlite/v1/discovery" || r.Header.Get("Authorization") != "synthetic-token" {
					t.Errorf("unexpected native request: %s %s, auth=%q", r.Method, r.URL, r.Header.Get("Authorization"))
				}
				discovery := Discovery{
					Format: 1, Kind: "etcqlite", ClusterID: "123", MemberID: "7",
					Revision: 4, SchemaVersion: 2, Capabilities: []string{"native-snapshot-v1"},
					SnapshotPath: "/etcqlite/v1/snapshots",
				}
				w.Header().Set("ETag", `"000000000000007b:0000000000000007"`)
				if scenario.change != nil {
					scenario.change(&discovery, w.Header())
				}
				_ = json.NewEncoder(w).Encode(discovery)
			}))
			defer server.Close()
			_, err := nativeDiscovery(context.Background(), server.Client(), server.URL, "synthetic-token", "123", "7")
			if scenario.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), scenario.wantError) {
				t.Fatalf("error = %v, want %q", err, scenario.wantError)
			}
		})
	}
}

func TestNativeSnapshotRequiresSamePreconditionAndPreservesPriorFile(t *testing.T) {
	image, _ := syntheticSnapshot()
	etag := `"000000000000007b:0000000000000007"`
	for _, code := range []int{http.StatusOK, http.StatusPreconditionFailed, http.StatusPreconditionRequired} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("If-Match") != etag || r.Header.Get("Authorization") != "synthetic-token" {
					t.Errorf("snapshot request did not pin discovery: %s %v", r.Method, r.Header)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
					t.Errorf("snapshot body must be {}: %v %v", body, err)
				}
				w.Header().Set("ETag", etag)
				w.Header().Set("Content-Type", "application/vnd.sqlite3")
				w.WriteHeader(code)
				_, _ = w.Write(image)
			}))
			defer server.Close()
			target := filepath.Join(t.TempDir(), "native.snap")
			discovery := Discovery{ETag: etag, SnapshotPath: "/etcqlite/v1/snapshots"}
			err := nativeSnapshot(context.Background(), server.Client(), server.URL, "synthetic-token", discovery, target)
			if code != http.StatusOK {
				if err == nil {
					t.Fatal("precondition failure was accepted")
				}
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatalf("failed download left a success-shaped artifact: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			first, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(first, image) {
				t.Fatalf("snapshot bytes changed: %v", err)
			}
			if err := nativeSnapshot(context.Background(), server.Client(), server.URL, "synthetic-token", discovery, target); err == nil {
				t.Fatal("existing native snapshot was overwritten")
			}
			after, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(after, first) {
				t.Fatal("previous recovery image was not preserved")
			}
		})
	}
}

func TestSnapshotVerificationRejectsCorruptionAndWrongIdentity(t *testing.T) {
	image, status := syntheticSnapshot()
	path := filepath.Join(t.TempDir(), "native.snap")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(path, status, "123", "7"); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(path, status, "124", "7"); err == nil {
		t.Fatal("foreign cluster snapshot was accepted")
	}
	status.SHA256 = strings.Repeat("0", 64)
	if err := verifySnapshot(path, status, "123", "7"); err == nil {
		t.Fatal("incorrect native status digest was accepted")
	}
	_, status = syntheticSnapshot()
	image[len(image)-1] ^= 1
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshot(path, status, "123", "7"); err == nil {
		t.Fatal("corrupt native trailer was accepted")
	}
}
