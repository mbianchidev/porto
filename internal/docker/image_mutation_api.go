package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (a *API) mutateImagePath(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(r.PathValue("id"), "/")
	switch {
	case strings.HasSuffix(path, "/tag"):
		r.SetPathValue("id", strings.TrimSuffix(path, "/tag"))
		a.tagImage(w, r)
	case strings.HasSuffix(path, "/push"):
		r.SetPathValue("id", strings.TrimSuffix(path, "/push"))
		a.pushImage(w, r)
	default:
		a.unsupported(w, r)
	}
}

func (a *API) tagImage(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("id")
	repository := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repository == "" {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "image tag repository is required"})
		return
	}
	target, err := taggedImageReference(repository, r.URL.Query().Get("tag"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if err := a.manager.TagImage(r.Context(), source, target); err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (a *API) loadImages(w http.ResponseWriter, r *http.Request) {
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
	err := a.manager.StreamLoadImages(r.Context(), r.Body, dockerBool(r, "quiet"), emit)
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
		_ = encoder.Encode(map[string]string{"stream": "Loaded image archive\n"})
	}
}

func (a *API) importImage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("fromSrc") != "-" {
		writeDockerUnsupported(w, "image import sources other than request body")
		return
	}
	reference, err := taggedImageReference(r.URL.Query().Get("repo"), r.URL.Query().Get("tag"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	wrote := false
	encoder := json.NewEncoder(w)
	emit := func(chunk runtimes.OutputChunk) error {
		if !wrote {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		return encoder.Encode(map[string]string{"status": strings.TrimSpace(string(chunk.Data))})
	}
	err = a.manager.StreamImportImage(
		r.Context(),
		r.Body,
		reference,
		r.URL.Query().Get("platform"),
		r.URL.Query().Get("message"),
		emit,
	)
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
		_ = encoder.Encode(map[string]string{"status": "Import complete"})
	}
}

func (a *API) pushImage(w http.ResponseWriter, r *http.Request) {
	reference, err := taggedImageReference(r.PathValue("id"), r.URL.Query().Get("tag"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	registryAuth, err := decodeRegistryAuthHeader(r.Header.Get("X-Registry-Auth"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	wrote := false
	encoder := json.NewEncoder(w)
	emit := func(chunk runtimes.OutputChunk) error {
		if !wrote {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			wrote = true
		}
		status := strings.TrimSpace(string(chunk.Data))
		if status == "" {
			return nil
		}
		return encoder.Encode(map[string]string{"status": status})
	}
	err = a.manager.StreamPushImageWithAuth(
		r.Context(),
		reference,
		r.URL.Query().Get("platform"),
		registryAuth,
		emit,
	)
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
		_ = encoder.Encode(map[string]string{"status": "Push complete"})
	}
}

func taggedImageReference(repository, tag string) (string, error) {
	repository = strings.TrimSpace(repository)
	tag = strings.TrimSpace(tag)
	if repository == "" || strings.ContainsAny(repository, "\r\n\x00") ||
		strings.ContainsAny(tag, "\r\n\x00") {
		return "", errors.New("invalid image reference")
	}
	if strings.Contains(repository, "@") {
		if tag != "" {
			return "", fmt.Errorf("%w: tag cannot be applied to a digest reference", ErrUnsupported)
		}
		return repository, nil
	}
	if tag == "" {
		tag = "latest"
	}
	slash := strings.LastIndexByte(repository, '/')
	if colon := strings.LastIndexByte(repository[slash+1:], ':'); colon >= 0 {
		repository = repository[:slash+1+colon]
	}
	return repository + ":" + tag, nil
}
