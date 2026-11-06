package datafiles

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestVersionOneManifestCompatibility(t *testing.T) {
	const legacyEmpty = `{"version":1,"resource":{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","readOnly":false},"createdAt":"2026-01-01T00:00:00Z","consistency":"crash-consistent","bytes":0,"entries":[],"sha256":""}`
	const legacy = `{"version":1,"resource":{"kind":"volume","name":"synthetic-volume","id":"synthetic-generation","readOnly":false},"createdAt":"2026-01-01T00:00:00Z","consistency":"crash-consistent","bytes":0,"entries":[{"path":"synthetic.txt","type":"file","size":0,"mode":416,"uid":0,"gid":0,"modifiedAt":"2026-01-01T00:00:00Z","sha256":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}],"sha256":""}`
	for _, test := range []struct {
		name      string
		document  string
		ownership string
		valid     bool
		empty     bool
	}{
		{name: "legacy empty archive", document: legacyEmpty, valid: true, empty: true},
		{name: "legacy ownership omitted", document: legacy, valid: true},
		{
			name: "filesystem ownership", ownership: "filesystem", valid: true,
			document: strings.Replace(legacy, `"bytes":0`, `"ownership":"filesystem","bytes":0`, 1),
		},
		{
			name: "namespace ownership", ownership: "namespace", valid: true,
			document: strings.Replace(legacy, `"bytes":0`, `"ownership":"namespace","bytes":0`, 1),
		},
		{name: "future version", document: strings.Replace(legacy, `"version":1`, `"version":2`, 1)},
		{name: "missing version", document: strings.Replace(legacy, `"version":1,`, "", 1)},
		{name: "unknown field", document: strings.Replace(legacy, `"bytes":0`, `"future":true,"bytes":0`, 1)},
		{name: "unknown ownership", document: strings.Replace(legacy, `"bytes":0`, `"ownership":"future","bytes":0`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			hash := sha256.Sum256([]byte(test.document))
			document := strings.Replace(test.document, `"sha256":""`, `"sha256":"`+hex.EncodeToString(hash[:])+`"`, 1)
			var raw bytes.Buffer
			writer := tar.NewWriter(&raw)
			if !test.empty {
				if err := writer.WriteHeader(&tar.Header{
					Name: "data/synthetic.txt", Mode: 0o640, Typeflag: tar.TypeReg,
					ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.WriteHeader(&tar.Header{
				Name: "porto-manifest.json", Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(len(document)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(writer, document); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			manifest, err := Validate(context.Background(), bytes.NewReader(raw.Bytes()))
			if !test.valid {
				if err == nil {
					t.Fatal("an unsupported manifest contract was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("version 1 archive is no longer readable: %v", err)
			}
			if manifest.Version != 1 || manifest.Ownership != test.ownership || manifest.SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatalf("archive contract changed: %+v", manifest)
			}
		})
	}
}

func TestArchiveRejectsUnsafeMembersBeforeRestore(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "/absolute", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../escape", Mode: 0o777},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
		{Name: "link", Typeflag: tar.TypeLink, Linkname: "../escape", Mode: 0o600},
		{Name: "device", Typeflag: tar.TypeChar, Mode: 0o600},
	} {
		t.Run(header.Name+header.Linkname, func(t *testing.T) {
			var raw bytes.Buffer
			writer := tar.NewWriter(&raw)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			var sealed bytes.Buffer
			if _, err := SealTar(context.Background(), &raw, &sealed, Resource{ID: "fixture"}); err == nil {
				t.Fatal("unsafe source archive was accepted")
			}
		})
	}
}

func TestRestorePreservesExistingDestinationOnFailure(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("fixture content"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "backup-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := Export(context.Background(), archive, source, Resource{ID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	original := filepath.Join(destination, "original")
	if err := os.WriteFile(original, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(context.Background(), archive, destination, RestoreOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected destination conflict, got %v", err)
	}
	content, err := os.ReadFile(original)
	if err != nil || string(content) != "untouched" {
		t.Fatal("existing volume data changed")
	}
	if _, err := archive.WriteAt([]byte("corrupted"), 512); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(context.Background(), archive); err == nil {
		t.Fatal("corrupted archive passed integrity validation")
	}
}

func TestArchiveLinksAndUnicode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows host symlink creation requires privileges; guest restore is tested on Linux")
	}
	source := t.TempDir()
	name := strings.Repeat("x", 150) + "-\u03b4.txt"
	if err := os.WriteFile(filepath.Join(source, name), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(source, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, name), filepath.Join(source, "hardlink")); err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "backup-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := Export(context.Background(), archive, source, Resource{ID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := Restore(context.Background(), archive, destination, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(destination, "symlink"))
	if err != nil || target != name {
		t.Fatalf("symlink=%q error=%v", target, err)
	}
	first, _ := os.Stat(filepath.Join(destination, name))
	second, _ := os.Stat(filepath.Join(destination, "hardlink"))
	if first == nil || second == nil || !os.SameFile(first, second) {
		t.Fatal("hardlink relationship was not preserved")
	}
	originalLink, err := os.Lstat(filepath.Join(source, "symlink"))
	if err != nil {
		t.Fatal(err)
	}
	restoredLink, err := os.Lstat(filepath.Join(destination, "symlink"))
	if err != nil || !restoredLink.ModTime().Equal(originalLink.ModTime()) {
		t.Fatalf("symlink timestamp was not preserved without following its target: %+v, %v", restoredLink, err)
	}
}

func TestArchiveCancellationAndLowSpace(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "data"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Export(ctx, io.Discard, source, Resource{ID: "fixture"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error=%v", err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "backup-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := Export(context.Background(), archive, source, Resource{ID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	err = Restore(context.Background(), archive, destination, RestoreOptions{
		AvailableBytes: func(string) (uint64, error) { return 1, nil },
	})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("expected insufficient-space error, got %v", err)
	}
	items, err := os.ReadDir(destination)
	if err != nil || len(items) != 0 {
		t.Fatal("low-space restore wrote destination data")
	}
}
func TestArchiveRoundTrip(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "fixture.txt"), []byte("synthetic volume\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	archive, err := os.CreateTemp(t.TempDir(), "backup-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	resource := Resource{Kind: "volume", Name: "fixture", ID: "synthetic-volume-1"}
	manifest, err := Export(context.Background(), archive, source, resource)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || manifest.Resource.ID != resource.ID || manifest.Bytes != int64(len("synthetic volume\n")) {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	if _, err := Validate(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := Restore(context.Background(), archive, destination, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "nested", "fixture.txt"))
	if err != nil || string(content) != "synthetic volume\n" {
		t.Fatalf("restored content=%q error=%v", content, err)
	}
	info, err := os.Stat(filepath.Join(destination, "nested", "fixture.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("permissions changed: %o", info.Mode().Perm())
	}
}

func TestRestoreRejectsValidArchiveChangedAfterPreview(t *testing.T) {
	source := t.TempDir()
	sourceFile := filepath.Join(source, "synthetic.txt")
	if err := os.WriteFile(sourceFile, []byte("previewed data"), 0o600); err != nil {
		t.Fatal(err)
	}
	resource := Resource{Kind: "volume", Name: "synthetic", ID: "volume-1"}
	var previewed bytes.Buffer
	if _, err := Export(context.Background(), &previewed, source, resource); err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(previewed.Bytes())
	if err := os.WriteFile(sourceFile, []byte("different valid data"), 0o600); err != nil {
		t.Fatal(err)
	}
	var changed bytes.Buffer
	if _, err := Export(context.Background(), &changed, source, resource); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(destination, "original.txt")
	if err := os.WriteFile(original, []byte("original stays intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ReplaceDirectory(context.Background(), destination, func(stage string) error {
		return Restore(context.Background(), bytes.NewReader(changed.Bytes()), stage, RestoreOptions{
			ExpectedSHA256: hex.EncodeToString(checksum[:]),
		})
	}, nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a different valid archive was not rejected: %v", err)
	}
	content, err := os.ReadFile(original)
	if err != nil || string(content) != "original stays intact" {
		t.Fatalf("restore changed original data: %q, %v", content, err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed restore left temporary staging behind: %+v, %v", entries, err)
	}
}
