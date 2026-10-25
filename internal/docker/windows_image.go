package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const maxWindowsImageMetadata = 4 * 1024 * 1024

type windowsImageMetadata struct {
	Image           struct{ Target ocispec.Descriptor }
	ImageConfigDesc ocispec.Descriptor
	ImageConfig     ocispec.Image
}

func (m *Manager) readWindowsImageMetadata(ctx context.Context, image string) (windowsImageMetadata, error) {
	var metadata windowsImageMetadata
	document, err := m.inspect(ctx, "image", image, "--mode", "native", "--platform", "windows/amd64")
	if err != nil {
		return metadata, err
	}
	if err := json.Unmarshal(document, &metadata); err != nil {
		return metadata, err
	}
	if err := metadata.ImageConfigDesc.Digest.Validate(); err != nil {
		return metadata, fmt.Errorf("invalid native Windows OCI configuration identity: %w", err)
	}
	if metadata.ImageConfig.OS != "windows" || metadata.ImageConfig.Architecture != "amd64" || metadata.ImageConfig.OSVersion == "" {
		return metadata, errors.New("invalid native Windows OCI image platform or Windows version")
	}
	if err := metadata.Image.Target.Digest.Validate(); err != nil {
		return metadata, fmt.Errorf("invalid native Windows OCI manifest target: %w", err)
	}
	if isImageDigest(image) && metadata.Image.Target.Digest.String() != image {
		return metadata, errors.New("invalid native Windows OCI image metadata: manifest identity changed")
	}
	return metadata, nil
}

func (m *Manager) nativeWindowsImages(ctx context.Context, images []Image) ([]Image, error) {
	metadataByTarget := make(map[string]windowsImageMetadata)
	for n := range images {
		target := images[n].Digest
		if !isImageDigest(target) {
			return nil, errors.New("invalid native Windows image listing: missing manifest identity")
		}
		metadata, ok := metadataByTarget[target]
		if !ok {
			var err error
			metadata, err = m.readWindowsImageMetadata(ctx, target)
			if err != nil {
				return nil, fmt.Errorf("read native Windows image listing metadata: %w", err)
			}
			metadataByTarget[target] = metadata
		}
		// nerdctl lists manifest IDs, whereas Docker exposes configuration IDs.
		images[n].ID = metadata.ImageConfigDesc.Digest.String()
		images[n].Platform = metadata.ImageConfig.OS + "/" + metadata.ImageConfig.Architecture
	}
	return images, nil
}

func windowsImageID(image string) bool {
	encoded := strings.TrimPrefix(image, "sha256:")
	return len(encoded) >= 12 && len(encoded) <= 64 && strings.Trim(encoded, "0123456789abcdef") == ""
}

func (m *Manager) windowsImageTarget(ctx context.Context, image string) (string, error) {
	if !windowsImageID(image) {
		return image, nil
	}
	images, err := m.Images(ctx)
	if err != nil {
		return "", err
	}
	var target string
	for _, candidate := range images {
		if !imageMatchesIdentifier(candidate, image) {
			continue
		}
		if !isImageDigest(candidate.Digest) {
			return "", errors.New("invalid native Windows image manifest identity")
		}
		if target != "" && target != candidate.Digest {
			return "", fmt.Errorf("%w: native Windows image ID %s is ambiguous", ErrConflict, image)
		}
		target = candidate.Digest
	}
	if target == "" {
		return "", fmt.Errorf("%w: native Windows image %s", ErrNotFound, image)
	}
	return target, nil
}

func (m *Manager) inspectWindowsImage(ctx context.Context, image, platform string) (json.RawMessage, error) {
	if platform != "" && platform != "windows/amd64" {
		return nil, errors.New("invalid native Windows image platform: inspect requires windows/amd64")
	}
	image = normalizeNerdctlReference(m.windowsImageReference(image))
	if strings.Contains(image, "@") {
		named, err := reference.ParseNormalizedNamed(image)
		if err != nil {
			return nil, fmt.Errorf("invalid native Windows image reference: %w", err)
		}
		canonical, ok := named.(reference.Canonical)
		if !ok {
			return nil, errors.New("invalid native Windows image digest reference")
		}
		// nerdctl's compatibility inspector mismatches empty-tag digest names.
		image = canonical.Digest().String()
	} else {
		var err error
		image, err = m.windowsImageTarget(ctx, image)
		if err != nil {
			return nil, err
		}
	}
	document, err := m.inspect(ctx, "image", image, "--platform", "windows/amd64")
	if err != nil {
		return nil, err
	}
	var inspected struct {
		ID         string              `json:"Id"`
		OSVersion  string              `json:"OsVersion"`
		Descriptor *ocispec.Descriptor `json:"Descriptor"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, err
	}
	if inspected.Descriptor != nil {
		if err := inspected.Descriptor.Digest.Validate(); err != nil {
			return nil, fmt.Errorf("invalid native Windows image target: %w", err)
		}
	}
	if inspected.OSVersion != "" && inspected.Descriptor != nil {
		return document, nil
	}
	// The compatibility schema omits os.version and the manifest descriptor.
	metadata, err := m.readWindowsImageMetadata(ctx, image)
	if err != nil {
		return nil, err
	}
	if metadata.ImageConfigDesc.Digest.String() != inspected.ID {
		return nil, errors.New("invalid native Windows OCI image metadata: configuration identity changed")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, err
	}
	fields["OsVersion"], err = json.Marshal(metadata.ImageConfig.OSVersion)
	if err != nil {
		return nil, err
	}
	fields["Descriptor"], err = json.Marshal(metadata.Image.Target)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (m *Manager) resolveWindowsImage(ctx context.Context, image string, auth *RegistryAuth) (windowscontainers.ImagePlatform, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	name = reference.TagNameOnly(name)
	resolver, err := m.ownerRegistryResolver(image, auth)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	resolved, descriptor, err := resolver.Resolve(ctx, name.String())
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	fetcher, err := resolver.Fetcher(ctx, resolved)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	metadata, manifest, err := selectWindowsManifest(ctx, fetcher, descriptor, m.windows.installation.Host, 0)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(name), manifest.Digest)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	return metadata, pinned.String(), nil
}

func fetchWindowsMetadata(ctx context.Context, fetcher remotes.Fetcher, descriptor ocispec.Descriptor) ([]byte, error) {
	if descriptor.Size <= 0 || descriptor.Size > maxWindowsImageMetadata {
		return nil, errors.New("Windows image metadata exceeds the bounded preflight size")
	}
	if err := descriptor.Digest.Validate(); err != nil {
		return nil, err
	}
	body, err := fetcher.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxWindowsImageMetadata+1))
	err = errors.Join(readErr, body.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != descriptor.Size || descriptor.Digest.Algorithm().FromBytes(data) != descriptor.Digest {
		return nil, errors.New("Windows image metadata length or digest differs from its registry descriptor")
	}
	return data, nil
}

func selectWindowsManifest(ctx context.Context, fetcher remotes.Fetcher, descriptor ocispec.Descriptor,
	host windowscontainers.Host, depth int) (windowscontainers.ImagePlatform, ocispec.Descriptor, error) {
	if depth > 2 {
		return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, errors.New("Windows image manifest nesting exceeds the preflight limit")
	}
	data, err := fetchWindowsMetadata(ctx, fetcher, descriptor)
	if err != nil {
		return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
	}
	switch descriptor.MediaType {
	case ocispec.MediaTypeImageIndex, "application/vnd.docker.distribution.manifest.list.v2+json":
		var index ocispec.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
		}
		var failures []error
		// Prefer the selected host's default isolation contract. Selection
		// never retries a mismatched Server process image with Hyper-V.
		for _, exact := range []bool{true, false} {
			for _, child := range index.Manifests {
				if child.Platform == nil {
					continue
				}
				platform := windowscontainers.ImagePlatform{OS: child.Platform.OS, Architecture: child.Platform.Architecture, OSVersion: child.Platform.OSVersion}
				if (host.ValidateImage(platform, "") == nil) != exact {
					continue
				}
				if err := validateWindowsPull(host, platform); err != nil {
					failures = append(failures, err)
					continue
				}
				metadata, selected, err := selectWindowsManifest(ctx, fetcher, child, host, depth+1)
				if err == nil {
					return metadata, selected, nil
				}
				failures = append(failures, err)
			}
		}
		return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, fmt.Errorf("no supported windows/amd64 image for host build %d: %w", host.Build, errors.Join(failures...))
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
		var manifest ocispec.Manifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
		}
		config, err := fetchWindowsMetadata(ctx, fetcher, manifest.Config)
		if err != nil {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
		}
		var image ocispec.Image
		if err := json.Unmarshal(config, &image); err != nil {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
		}
		platform := windowscontainers.ImagePlatform{OS: image.OS, Architecture: image.Architecture, OSVersion: image.OSVersion}
		if err := validateWindowsPull(host, platform); err != nil {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, err
		}
		if descriptor.Platform != nil && (descriptor.Platform.OS != image.OS || descriptor.Platform.Architecture != image.Architecture ||
			descriptor.Platform.OSVersion != image.OSVersion) {
			return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, errors.New("Windows image manifest platform differs from its config")
		}
		return platform, descriptor, nil
	default:
		return windowscontainers.ImagePlatform{}, ocispec.Descriptor{}, fmt.Errorf("unsupported Windows image manifest media type %q", descriptor.MediaType)
	}
}
