package docker

import (
	"archive/tar"
	"encoding/json"
	"os"
	"runtime"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPlatformImageArchiveSelectsRuntimeManifest(t *testing.T) {
	selectedDigest := digest.Digest("sha256:" + repeatHex("1"))
	otherDigest := digest.Digest("sha256:" + repeatHex("2"))
	nestedDigest := digest.Digest("sha256:" + repeatHex("3"))
	nested := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    otherDigest,
				Platform:  &ocispec.Platform{OS: "linux", Architecture: "other"},
			},
			{
				MediaType: ocispec.MediaTypeImageManifest,
				Digest:    selectedDigest,
				Platform:  &ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH},
			},
		},
	}
	root := ocispec.Index{
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageIndex,
			Digest:    nestedDigest,
			Annotations: map[string]string{
				"org.opencontainers.image.ref.name": "latest",
			},
		}},
	}
	archive, err := os.CreateTemp(t.TempDir(), "images-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(archive)
	writeJSONTarEntry(t, writer, "blobs/sha256/"+nestedDigest.Encoded(), nested)
	writeJSONTarEntry(t, writer, "index.json", root)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	normalized, err := platformImageArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer normalized.Close()
	if normalized == archive {
		t.Fatal("multi-platform archive was not normalized")
	}
	index, err := readImageArchiveIndex(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != selectedDigest {
		t.Fatalf("normalized index = %+v", index.Manifests)
	}
	if index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "latest" {
		t.Fatalf("normalized annotations = %+v", index.Manifests[0].Annotations)
	}
}

func writeJSONTarEntry(t *testing.T, writer *tar.Writer, name string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
}

func repeatHex(value string) string {
	result := ""
	for range 64 {
		result += value
	}
	return result
}
