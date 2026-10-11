package datafiles

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
