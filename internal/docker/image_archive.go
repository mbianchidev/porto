package docker

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func imageArchiveConfigDigest(archive *os.File, source string, platform ocispec.Platform) (string, error) {
	current := source
	for depth := 0; depth < 8; depth++ {
		algorithm, encoded, ok := strings.Cut(current, ":")
		if !ok || algorithm != "sha256" || len(encoded) != 64 {
			return "", errors.New("source image has an invalid SHA-256 identity")
		}
		var document []byte
		for _, name := range []string{"blobs/sha256/" + encoded, encoded + ".json"} {
			if _, err := archive.Seek(0, io.SeekStart); err != nil {
				return "", err
			}
			reader := tar.NewReader(archive)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					return "", err
				}
				if header.Name != name {
					continue
				}
				if header.Size < 0 || header.Size > 16*1024*1024 || header.Typeflag != tar.TypeReg {
					return "", errors.New("image identity member is not bounded regular JSON")
				}
				document, err = io.ReadAll(reader)
				if err != nil {
					return "", err
				}
				break
			}
			if document != nil {
				break
			}
		}
		if document == nil {
			return "", fmt.Errorf("image archive does not contain source identity %s", current)
		}
		sum := sha256.Sum256(document)
		if hex.EncodeToString(sum[:]) != encoded {
			return "", errors.New("image archive identity member checksum does not match")
		}
		var node struct {
			Config    ocispec.Descriptor
			Manifests []ocispec.Descriptor
			RootFS    json.RawMessage `json:"rootfs"`
		}
		if err := json.Unmarshal(document, &node); err != nil {
			return "", err
		}
		if node.RootFS != nil {
			return current, nil
		}
		if node.Config.Digest != "" {
			current = node.Config.Digest.String()
			continue
		}
		matched := false
		for _, manifest := range node.Manifests {
			if manifest.Platform != nil && manifest.Platform.OS == platform.OS && manifest.Platform.Architecture == platform.Architecture &&
				(platform.Variant == "" || manifest.Platform.Variant == platform.Variant) {
				current = manifest.Digest.String()
				matched = true
				break
			}
		}
		if !matched && len(node.Manifests) == 1 {
			current = node.Manifests[0].Digest.String()
			matched = true
		}
		if !matched {
			return "", errors.New("image archive does not resolve the selected source platform")
		}
	}
	return "", errors.New("image archive identity nesting exceeds its limit")
}

var errImageArchiveNoIndex = errors.New("image archive has no index.json")

func platformImageArchive(archive *os.File) (*os.File, error) {
	root, err := readImageArchiveIndex(archive)
	if err != nil {
		if errors.Is(err, errImageArchiveNoIndex) {
			if _, seekErr := archive.Seek(0, io.SeekStart); seekErr != nil {
				return nil, fmt.Errorf("rewind image archive: %w", seekErr)
			}
			return archive, nil
		}
		return nil, err
	}
	if len(root.Manifests) != 1 || root.Manifests[0].MediaType != ocispec.MediaTypeImageIndex {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind image archive: %w", err)
		}
		return archive, nil
	}
	nested, err := readImageArchiveIndexBlob(archive, root.Manifests[0].Digest.Encoded())
	if err != nil {
		return nil, err
	}
	var selected *ocispec.Descriptor
	for index := range nested.Manifests {
		descriptor := &nested.Manifests[index]
		if descriptor.Platform != nil &&
			descriptor.Platform.OS == "linux" &&
			(descriptor.Platform.Architecture == runtime.GOARCH || len(nested.Manifests) == 1) {
			selected = descriptor
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("image archive does not contain linux/%s", runtime.GOARCH)
	}
	descriptor := *selected
	descriptor.Annotations = cloneStringMap(root.Manifests[0].Annotations)
	root.Manifests = []ocispec.Descriptor{descriptor}
	indexJSON, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode platform image index: %w", err)
	}
	return rewriteImageArchiveIndex(archive, indexJSON)
}

func readImageArchiveIndex(archive *os.File) (ocispec.Index, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return ocispec.Index{}, fmt.Errorf("rewind image archive: %w", err)
	}
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return ocispec.Index{}, errImageArchiveNoIndex
		}
		if err != nil {
			return ocispec.Index{}, fmt.Errorf("read image archive: %w", err)
		}
		if header.Name != "index.json" {
			continue
		}
		var index ocispec.Index
		if err := json.NewDecoder(io.LimitReader(reader, 16*1024*1024)).Decode(&index); err != nil {
			return ocispec.Index{}, fmt.Errorf("decode image archive index: %w", err)
		}
		return index, nil
	}
}

func readImageArchiveIndexBlob(archive *os.File, encodedDigest string) (ocispec.Index, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return ocispec.Index{}, fmt.Errorf("rewind image archive: %w", err)
	}
	name := "blobs/sha256/" + encodedDigest
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return ocispec.Index{}, fmt.Errorf("image archive is missing %s", name)
		}
		if err != nil {
			return ocispec.Index{}, fmt.Errorf("read image archive: %w", err)
		}
		if header.Name != name {
			continue
		}
		var index ocispec.Index
		if err := json.NewDecoder(io.LimitReader(reader, 16*1024*1024)).Decode(&index); err != nil {
			return ocispec.Index{}, fmt.Errorf("decode image archive platform index: %w", err)
		}
		return index, nil
	}
}

func rewriteImageArchiveIndex(archive *os.File, indexJSON []byte) (*os.File, error) {
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind image archive: %w", err)
	}
	output, err := os.CreateTemp("", "porto-platform-images-*.tar")
	if err != nil {
		return nil, fmt.Errorf("create platform image archive: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = output.Close()
			_ = os.Remove(output.Name())
		}
	}()
	reader := tar.NewReader(archive)
	writer := tar.NewWriter(output)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read image archive: %w", err)
		}
		copyHeader := *header
		content := io.Reader(reader)
		if header.Name == "index.json" {
			copyHeader.Size = int64(len(indexJSON))
			content = strings.NewReader(string(indexJSON))
		}
		if err := writer.WriteHeader(&copyHeader); err != nil {
			return nil, fmt.Errorf("write platform image archive header: %w", err)
		}
		if _, err := io.CopyN(writer, content, copyHeader.Size); err != nil {
			return nil, fmt.Errorf("write platform image archive: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close platform image archive: %w", err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind platform image archive: %w", err)
	}
	cleanup = false
	return output, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
