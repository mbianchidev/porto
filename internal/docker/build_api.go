package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const maxBuildContextBytes = int64(2 * 1024 * 1024 * 1024)

func (a *API) buildImage(w http.ResponseWriter, r *http.Request) {
	request, err := dockerBuildRequest(r)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	archivePath, err := storeDockerBuildContext(w, r)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	defer os.Remove(archivePath)
	request.ContextArchive = archivePath

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

func storeDockerBuildContext(w http.ResponseWriter, r *http.Request) (string, error) {
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" && encoding != "x-gzip" {
		return "", fmt.Errorf("%w: build context encoding %s", ErrUnsupported, encoding)
	}
	archive, err := os.CreateTemp("", "porto-build-context-*.tar")
	if err != nil {
		return "", fmt.Errorf("create build context: %w", err)
	}
	path := archive.Name()
	cleanup := true
	defer func() {
		_ = archive.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if err := archive.Chmod(0o600); err != nil {
		return "", fmt.Errorf("protect build context: %w", err)
	}
	count, err := io.Copy(archive, http.MaxBytesReader(w, r.Body, maxBuildContextBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return "", errors.New("build context exceeds the 2 GiB limit")
		}
		return "", fmt.Errorf("store build context: %w", err)
	}
	if count == 0 {
		return "", errors.New("invalid build context: empty")
	}
	if err := archive.Close(); err != nil {
		return "", fmt.Errorf("close build context: %w", err)
	}
	cleanup = false
	return path, nil
}
