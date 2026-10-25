package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/containerd/v2/core/remotes"
	registrydocker "github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/registries"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const maxWindowsImageMetadata = 4 * 1024 * 1024

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
	}
	document, err := m.inspect(ctx, "image", image, "--platform", "windows/amd64")
	if err != nil {
		return nil, err
	}
	var inspected struct {
		ID        string `json:"Id"`
		OSVersion string `json:"OsVersion"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, err
	}
	if inspected.OSVersion != "" {
		return document, nil
	}
	// The pinned nerdctl compatibility schema omits Windows' os.version.
	native, err := m.inspect(ctx, "image", image, "--mode", "native", "--platform", "windows/amd64")
	if err != nil {
		return nil, err
	}
	var metadata struct {
		ImageConfigDesc ocispec.Descriptor
		ImageConfig     ocispec.Image
	}
	if err := json.Unmarshal(native, &metadata); err != nil {
		return nil, err
	}
	if metadata.ImageConfigDesc.Digest.String() != inspected.ID || inspected.ID == "" ||
		metadata.ImageConfig.OS != "windows" || metadata.ImageConfig.Architecture != "amd64" || metadata.ImageConfig.OSVersion == "" {
		return nil, errors.New("invalid native Windows OCI image metadata: configuration identity, platform, or Windows version differs")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, err
	}
	fields["OsVersion"], err = json.Marshal(metadata.ImageConfig.OSVersion)
	if err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

func (m *Manager) resolveWindowsImage(ctx context.Context, image string, auth *RegistryAuth) (windowscontainers.ImagePlatform, string, error) {
	name, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	name = reference.TagNameOnly(name)
	username, secret := "", ""
	if auth != nil {
		if auth.RegistryToken != "" {
			return windowscontainers.ImagePlatform{}, "", fmt.Errorf("%w: raw registry tokens for native image preflight", ErrUnsupported)
		}
		username, secret = auth.Username, auth.Password
		if auth.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
			if err != nil {
				return windowscontainers.ImagePlatform{}, "", err
			}
			var ok bool
			username, secret, ok = strings.Cut(string(decoded), ":")
			if !ok {
				return windowscontainers.ImagePlatform{}, "", errors.New("invalid registry authentication")
			}
		}
		if auth.IdentityToken != "" {
			username, secret = "", auth.IdentityToken
		}
	}
	registry, err := registries.ServerForImage(image)
	if err != nil {
		return windowscontainers.ImagePlatform{}, "", err
	}
	if auth != nil && auth.ServerAddress != "" {
		server, err := registries.NormalizeServer(auth.ServerAddress)
		if err != nil || server != registry {
			return windowscontainers.ImagePlatform{}, "", errors.New("image preflight credentials do not belong to this registry")
		}
	}
	allowedHost := reference.Domain(name)
	if allowedHost == "docker.io" {
		allowedHost = "registry-1.docker.io"
	}
	client := registryHTTPClient()
	defer client.CloseIdleConnections()
	resolver := registrydocker.NewResolver(registrydocker.ResolverOptions{
		Client: client,
		Credentials: func(host string) (string, string, error) {
			if host != allowedHost {
				return "", "", errors.New("image preflight requested credentials for another registry")
			}
			return username, secret, nil
		},
	})
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
