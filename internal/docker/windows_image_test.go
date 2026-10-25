package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type windowsMetadataFetcher map[digest.Digest][]byte

func (fetcher windowsMetadataFetcher) Fetch(_ context.Context, descriptor ocispec.Descriptor) (io.ReadCloser, error) {
	data, ok := fetcher[descriptor.Digest]
	if !ok {
		return nil, fmt.Errorf("unexpected metadata request for %s; layers must not be downloaded during preflight", descriptor.Digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func addWindowsMetadata(t *testing.T, fetcher windowsMetadataFetcher, mediaType string, value any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	id := digest.FromBytes(data)
	fetcher[id] = data
	return ocispec.Descriptor{Digest: id, Size: int64(len(data)), MediaType: mediaType}
}

func TestNativeWindowsRegistryPreflightSelectsMatchingBuildWithoutLayers(t *testing.T) {
	fetcher := windowsMetadataFetcher{}
	makeImage := func(os, arch, version string) ocispec.Descriptor {
		platform := ocispec.Platform{OS: os, Architecture: arch, OSVersion: version}
		config := addWindowsMetadata(t, fetcher, ocispec.MediaTypeImageConfig, ocispec.Image{Platform: platform})
		manifest := addWindowsMetadata(t, fetcher, ocispec.MediaTypeImageManifest, ocispec.Manifest{
			Versioned: specs.Versioned{SchemaVersion: 2}, Config: config,
			Layers: []ocispec.Descriptor{{Digest: digest.FromString("must not download this synthetic layer"), Size: 12345}},
		})
		manifest.Platform = &platform
		return manifest
	}
	wrongBuild := makeImage("windows", "amd64", "10.0.20348.1")
	linux := makeImage("linux", "amd64", "")
	arm := makeImage("windows", "arm64", "10.0.26100.1")
	matching := makeImage("windows", "amd64", "10.0.26100.1000")
	index := addWindowsMetadata(t, fetcher, ocispec.MediaTypeImageIndex, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{wrongBuild, linux, arm, matching},
	})
	host := nativeTestInstallation(t).Host
	host.HyperV, host.Virtualization = true, true
	platform, selected, err := selectWindowsManifest(context.Background(), fetcher, index, host, 0)
	if err != nil || selected.Digest != matching.Digest || platform.OSVersion != "10.0.26100.1000" {
		t.Fatalf("selected native metadata = %+v, %+v, %v", platform, selected, err)
	}
	// An index is only a hint. The selected manifest's verified config remains
	// authoritative, and a digest mismatch is never treated as a platform skip.
	matching.Platform.OSVersion = "10.0.20348.1"
	if _, _, err := selectWindowsManifest(context.Background(), fetcher, matching, host, 0); err == nil {
		t.Fatal("accepted an index/config platform mismatch")
	}
	fetcher[index.Digest] = []byte("synthetic tampering")
	if _, _, err := selectWindowsManifest(context.Background(), fetcher, index, host, 0); err == nil {
		t.Fatal("accepted tampered registry metadata")
	}
}

func TestNativeWindowsRegistryPreflightRejectsUnsupportedConfig(t *testing.T) {
	for _, platform := range []ocispec.Platform{
		{OS: "linux", Architecture: "amd64"},
		{OS: "windows", Architecture: "arm64", OSVersion: "10.0.26100.1"},
		{OS: "windows", Architecture: "amd64", OSVersion: "10.0.20348.1"},
		{OS: "windows", Architecture: "amd64", OSVersion: ""},
	} {
		fetcher := windowsMetadataFetcher{}
		config := addWindowsMetadata(t, fetcher, ocispec.MediaTypeImageConfig, ocispec.Image{Platform: platform})
		manifest := addWindowsMetadata(t, fetcher, ocispec.MediaTypeImageManifest, ocispec.Manifest{Config: config})
		if _, _, err := selectWindowsManifest(context.Background(), fetcher, manifest, nativeTestInstallation(t).Host, 0); err == nil {
			t.Fatalf("accepted unsupported native image %+v", platform)
		}
	}
}
