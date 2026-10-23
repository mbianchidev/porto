package docker

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMigrationImageArchiveValidatesLegacyAndOCIConfigIdentityBeforeLoad(t *testing.T) {
	config := []byte(`{"architecture":"arm64","os":"linux","config":{"Env":["SYNTHETIC_MODE=fixture"]},"rootfs":{"type":"layers","diff_ids":[]}}`)
	configSum := sha256.Sum256(config)
	configID := "sha256:" + hex.EncodeToString(configSum[:])
	manifest, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "config": map[string]any{"mediaType": ocispec.MediaTypeImageConfig, "digest": configID, "size": len(config)}, "layers": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestSum := sha256.Sum256(manifest)
	manifestID := "sha256:" + hex.EncodeToString(manifestSum[:])
	for _, test := range []struct {
		name, identity, configName string
		corrupt                    bool
	}{
		{"legacy config digest", configID, configID[7:] + ".json", false},
		{"OCI manifest digest", manifestID, "blobs/sha256/" + configID[7:], false},
		{"corrupt config member", manifestID, "blobs/sha256/" + configID[7:], true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "synthetic-image-*.tar")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			writer := tar.NewWriter(file)
			data := config
			if test.corrupt {
				data = []byte(`{"rootfs":{"type":"layers"},"synthetic":"changed"}`)
			}
			for name, contents := range map[string][]byte{test.configName: data, "blobs/sha256/" + manifestID[7:]: manifest} {
				if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(contents))}); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(contents); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			actual, err := imageArchiveConfigDigest(file, test.identity, ocispec.Platform{OS: "linux", Architecture: "arm64"})
			if test.corrupt {
				if err == nil {
					t.Fatal("corrupt image configuration passed pre-load integrity checks")
				}
			} else if err != nil || actual != configID {
				t.Fatalf("source image identity did not bind its exact config bytes: %s, %v", actual, err)
			}
		})
	}
}
