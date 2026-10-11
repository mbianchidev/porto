package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildContextExtractionRejectsEscapePathsAndLinks(t *testing.T) {
	for _, header := range []*tar.Header{
		{Name: "../outside", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: "/outside", Mode: 0o644, Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside"},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/outside"},
		{Name: "link", Typeflag: tar.TypeLink, Linkname: "../outside"},
		{Name: "device", Typeflag: tar.TypeChar},
	} {
		t.Run(header.Name+header.Linkname, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractDockerBuildContext(context.Background(), t.TempDir(), &data); err == nil {
				t.Fatal("unsafe build archive was accepted")
			}
		})
	}
}

func TestBuildContextExtractionPreservesRegularFilesWithoutGlobalPaths(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	contents := "synthetic build fixture\n"
	if err := writer.WriteHeader(&tar.Header{Name: "nested/fixture", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(contents))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(contents)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := extractDockerBuildContext(context.Background(), directory, &data); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(directory, "nested", "fixture"))
	if err != nil || string(stored) != contents {
		t.Fatalf("extracted fixture = %q, error=%v", stored, err)
	}
}
