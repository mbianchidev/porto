package imageanalysis

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func archiveManifest(archive *os.File, image Image) (string, []string, error) {
	selectedPlatform, err := NormalizePlatform(image.Platform)
	if err != nil {
		return "", nil, err
	}
	current := image.Digest
	for depth := 0; depth < 8; depth++ {
		data, err := archiveBlob(archive, current)
		if err != nil {
			return "", nil, err
		}
		var node struct {
			Manifests []ocispec.Descriptor
			Config    ocispec.Descriptor
			Layers    []ocispec.Descriptor
		}
		if err := json.Unmarshal(data, &node); err != nil {
			return "", nil, errors.New("image manifest is not valid JSON")
		}
		if node.Config.Digest != "" {
			configuration, err := archiveBlob(archive, node.Config.Digest.String())
			if err != nil {
				return "", nil, err
			}
			var platform ocispec.Platform
			if err := json.Unmarshal(configuration, &platform); err != nil {
				return "", nil, errors.New("image platform configuration is invalid")
			}
			actual := platform.OS + "/" + platform.Architecture
			if platform.Variant != "" {
				actual += "/" + platform.Variant
			}
			normalized, err := NormalizePlatform(actual)
			if err != nil || normalized != selectedPlatform {
				return "", nil, fmt.Errorf("archive platform %s does not match selected platform %s", actual, image.Platform)
			}
			layers := make([]string, 0, len(node.Layers))
			for _, layer := range node.Layers {
				layers = append(layers, layer.Digest.String())
			}
			if err := verifyArchiveLayers(archive, layers); err != nil {
				return current, layers, err
			}
			return current, layers, nil
		}
		selected := ""
		for _, manifest := range node.Manifests {
			if manifest.Platform == nil {
				continue
			}
			platform := manifest.Platform.OS + "/" + manifest.Platform.Architecture
			if manifest.Platform.Variant != "" {
				platform += "/" + manifest.Platform.Variant
			}
			normalized, err := NormalizePlatform(platform)
			if err == nil && normalized == selectedPlatform {
				if selected != "" {
					return "", nil, errors.New("image index contains ambiguous platform manifests")
				}
				selected = manifest.Digest.String()
			}
		}
		if selected == "" {
			return "", nil, errors.New("selected platform manifest is unavailable locally; pull that platform before scanning")
		}
		current = selected
	}
	return "", nil, errors.New("image manifest nesting exceeds the limit")
}

func archiveBlob(archive *os.File, identity string) ([]byte, error) {
	parsed, err := digest.Parse(identity)
	if err != nil || parsed.Algorithm() != digest.SHA256 {
		return nil, errors.New("image analysis requires a full immutable SHA-256 manifest digest")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("local OCI archive lacks manifest/config %s; legacy or partially downloaded images cannot establish complete analysis", identity)
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(header.Name, "./") != "blobs/sha256/"+parsed.Encoded() {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > 16*1024*1024 {
			return nil, errors.New("image manifest/config is not bounded regular JSON")
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != parsed.Encoded() {
			return nil, errors.New("local image manifest/config checksum does not match its identity")
		}
		return data, nil
	}
}

func verifyArchiveLayers(archive *os.File, layers []string) error {
	pending := map[string]string{}
	for _, layer := range layers {
		parsed, err := digest.Parse(layer)
		if err != nil || parsed.Algorithm() != digest.SHA256 {
			return errors.New("image layer has an unsupported digest")
		}
		pending["blobs/sha256/"+parsed.Encoded()] = parsed.Encoded()
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := tar.NewReader(archive)
	for len(pending) > 0 {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("local image is partially downloaded: one or more selected layers are unavailable")
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(header.Name, "./")
		expected, required := pending[name]
		if !required {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return errors.New("image layer is not a regular archive member")
		}
		hasher := sha256.New()
		if _, err := io.Copy(hasher, reader); err != nil {
			return err
		}
		if hex.EncodeToString(hasher.Sum(nil)) != expected {
			return errors.New("local image layer checksum does not match")
		}
		delete(pending, name)
	}
	return nil
}
