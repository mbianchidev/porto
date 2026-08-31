package docker

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	maxBuildContextBytes = int64(2 * 1024 * 1024 * 1024)
	maxBuildContextFiles = 100000
)

func (a *API) buildImage(w http.ResponseWriter, r *http.Request) {
	request, err := dockerBuildRequest(r)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	contextDirectory, err := extractDockerBuildContext(w, r)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	defer os.RemoveAll(contextDirectory)
	request.Context = contextDirectory

	wrote := false
	encoder := json.NewEncoder(w)
	emit := func(chunk runtimes.OutputChunk) error {
		if !wrote {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		return encoder.Encode(map[string]string{"stream": string(chunk.Data)})
	}
	err = a.manager.StreamBuild(r.Context(), request, emit)
	if err != nil {
		if !wrote {
			writeDockerError(w, err)
			return
		}
		_ = encoder.Encode(map[string]any{
			"error":       err.Error(),
			"errorDetail": map[string]string{"message": err.Error()},
		})
		return
	}
	if !wrote {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = encoder.Encode(map[string]string{"stream": ""})
	}
}

func dockerBuildRequest(r *http.Request) (BuildRequest, error) {
	query := r.URL.Query()
	for _, key := range []string{
		"remote", "extrahosts", "q", "squash", "outputs",
		"memory", "memswap", "cpushares", "cpusetcpus", "cpuperiod", "cpuquota", "shmsize",
	} {
		if value := strings.TrimSpace(query.Get(key)); value != "" && value != "0" && value != "false" {
			return BuildRequest{}, fmt.Errorf("%w: Docker build option %s", ErrUnsupported, key)
		}
	}
	var buildArgs map[string]*string
	if err := decodeBuildQueryJSON(query.Get("buildargs"), &buildArgs); err != nil {
		return BuildRequest{}, fmt.Errorf("invalid buildargs: %w", err)
	}
	var labels map[string]string
	if err := decodeBuildQueryJSON(query.Get("labels"), &labels); err != nil {
		return BuildRequest{}, fmt.Errorf("invalid build labels: %w", err)
	}
	var cacheFrom []string
	if err := decodeBuildQueryJSON(query.Get("cachefrom"), &cacheFrom); err != nil {
		return BuildRequest{}, fmt.Errorf("invalid cachefrom: %w", err)
	}
	return BuildRequest{
		Dockerfile: firstNonEmpty(query.Get("dockerfile"), "Dockerfile"),
		Tags:       append([]string(nil), query["t"]...),
		Target:     query.Get("target"),
		Platform:   query.Get("platform"),
		Network:    query.Get("networkmode"),
		NoCache:    dockerBool(r, "nocache"),
		Pull:       dockerBool(r, "pull"),
		BuildArgs:  buildArgs,
		Labels:     labels,
		CacheFrom:  cacheFrom,
	}, nil
}

func decodeBuildQueryJSON[T any](raw string, destination *T) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), destination)
}

func extractDockerBuildContext(w http.ResponseWriter, r *http.Request) (string, error) {
	directory, err := os.MkdirTemp("", "porto-build-context-*")
	if err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()

	reader := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxBuildContextBytes))
	var archiveReader io.Reader = reader
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	compressed, _ := reader.Peek(2)
	if encoding == "gzip" || (len(compressed) == 2 && compressed[0] == 0x1f && compressed[1] == 0x8b) {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			return "", fmt.Errorf("read compressed build context: %w", err)
		}
		defer gzipReader.Close()
		archiveReader = gzipReader
	} else if encoding != "" && encoding != "identity" {
		return "", fmt.Errorf("%w: build context encoding %s", ErrUnsupported, encoding)
	}

	tarReader := tar.NewReader(archiveReader)
	var extractedBytes int64
	files := 0
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read build context archive: %w", err)
		}
		files++
		if files > maxBuildContextFiles {
			return "", errors.New("build context contains too many files")
		}
		extractedBytes += header.Size
		if header.Size < 0 || extractedBytes > maxBuildContextBytes {
			return "", errors.New("build context exceeds the 2 GiB limit")
		}
		target, err := safeBuildContextPath(directory, header.Name)
		if err != nil {
			return "", err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)&0o777); err != nil {
				return "", fmt.Errorf("create build context directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := ensureBuildContextParents(directory, filepath.Dir(target)); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("create build context parent: %w", err)
			}
			if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
				return "", fmt.Errorf("invalid unsafe build context path %q", header.Name)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("inspect build context path: %w", err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return "", fmt.Errorf("create build context file: %w", err)
			}
			if _, err := io.CopyN(file, tarReader, header.Size); err != nil {
				_ = file.Close()
				return "", fmt.Errorf("extract build context file: %w", err)
			}
			if err := file.Close(); err != nil {
				return "", fmt.Errorf("close build context file: %w", err)
			}
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeSymlink, tar.TypeLink:
			return "", fmt.Errorf("%w: build context links", ErrUnsupported)
		default:
			return "", fmt.Errorf("%w: build context archive entry type %s", ErrUnsupported, strconv.Itoa(int(header.Typeflag)))
		}
	}
	if files == 0 {
		return "", errors.New("build context is empty")
	}
	cleanup = false
	return directory, nil
}

func safeBuildContextPath(root, name string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(name, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) ||
		filepath.VolumeName(filepath.FromSlash(clean)) != "" {
		return "", fmt.Errorf("invalid unsafe build context path %q", name)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid unsafe build context path %q", name)
	}
	return target, nil
}

func ensureBuildContextParents(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil {
		return fmt.Errorf("inspect build context parent: %w", err)
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect build context parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid unsafe build context path through symbolic link %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("invalid unsafe build context parent %q", current)
		}
	}
	return nil
}
