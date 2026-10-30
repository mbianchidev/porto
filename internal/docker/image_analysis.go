package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mbianchidev/porto/internal/imageanalysis"
	"github.com/mbianchidev/porto/internal/localtools"
	digest "github.com/opencontainers/go-digest"
)

func (m *Manager) ResolveAnalysisImage(ctx context.Context, reference, platform string) (imageanalysis.Image, error) {
	if m.IsNativeWindows() {
		return imageanalysis.Image{}, fmt.Errorf("%w: local analysis currently requires a Linux OCI image", ErrUnsupported)
	}
	if err := validateObjectID(reference); err != nil {
		return imageanalysis.Image{}, err
	}
	if platform != "" {
		var err error
		platform, err = imageanalysis.NormalizePlatform(platform)
		if err != nil {
			return imageanalysis.Image{}, err
		}
	}
	images, err := m.Images(ctx)
	if err != nil {
		return imageanalysis.Image{}, err
	}
	for _, image := range images {
		if !imageMatchesIdentifier(image, reference) {
			continue
		}
		parsed, err := digest.Parse(image.Digest)
		if err != nil || parsed.Algorithm() != digest.SHA256 {
			return imageanalysis.Image{}, errors.New("local image has no full immutable manifest digest")
		}
		document, err := m.InspectImage(ctx, image.Digest, platform)
		if err != nil {
			return imageanalysis.Image{}, err
		}
		var metadata struct{ OS, Architecture, Variant string }
		if err := json.Unmarshal(document, &metadata); err != nil {
			return imageanalysis.Image{}, err
		}
		actual := metadata.OS + "/" + metadata.Architecture
		if metadata.Variant != "" {
			actual += "/" + metadata.Variant
		}
		if metadata.OS != "linux" || metadata.Architecture == "" {
			return imageanalysis.Image{}, fmt.Errorf("%w: image analysis requires an identifiable Linux platform", ErrUnsupported)
		}
		actual, err = imageanalysis.NormalizePlatform(actual)
		if err != nil {
			return imageanalysis.Image{}, err
		}
		if platform != "" && platform != actual {
			return imageanalysis.Image{}, errors.New("selected image platform is unavailable locally")
		}
		return imageanalysis.Image{Digest: image.Digest, Platform: actual}, nil
	}
	return imageanalysis.Image{}, ErrNotFound
}

func (m *Manager) ExportAnalysisImage(ctx context.Context, image imageanalysis.Image, destination io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !strings.HasPrefix(image.Platform, "linux/") {
		return errors.New("analysis export requires an explicit Linux platform")
	}
	process, err := m.startImageSave(ctx, []string{image.Digest}, image.Platform)
	if err != nil {
		return err
	}
	defer process.Stdout().Close()
	defer process.Stderr().Close()
	if err := process.Stdin().Close(); err != nil {
		cancel()
		return errors.Join(err, process.Wait())
	}
	diagnosticsDone := make(chan error, 1)
	go func() {
		_, err := localtools.CopyBounded(io.Discard, process.Stderr(), 64*1024)
		if err != nil {
			cancel()
		}
		diagnosticsDone <- err
	}()
	_, copyErr := localtools.CopyBounded(destination, process.Stdout(), imageanalysis.MaxArchiveBytes)
	if copyErr != nil {
		cancel()
	}
	diagnosticsErr := <-diagnosticsDone
	if err := errors.Join(copyErr, diagnosticsErr, process.Wait()); err != nil {
		return fmt.Errorf("export selected local image for analysis: %w", err)
	}
	return nil
}
