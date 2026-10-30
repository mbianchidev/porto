package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/resources"
)

func (a *API) imageHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validateObjectID(id); err != nil {
		writeDockerError(w, err)
		return
	}
	output, err := a.manager.runStdout(r.Context(), "read image history",
		"history", "--no-trunc", "--human=false", "--format", "{{json .}}", normalizeNerdctlReference(id))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	type historyItem struct {
		ID        string `json:"Id"`
		Created   int64
		CreatedBy string
		Tags      []string
		Size      int64
		Comment   string
	}
	result := make([]historyItem, 0)
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var item struct{ Snapshot, CreatedAt, CreatedBy, Size, Comment string }
		if err := decoder.Decode(&item); err == io.EOF {
			break
		} else if err != nil {
			writeDockerError(w, fmt.Errorf("decode image history: %w", err))
			return
		}
		size, err := strconv.ParseInt(item.Size, 10, 64)
		if err != nil {
			size, err = resources.ParseBytes(item.Size)
		}
		if err != nil || size < 0 {
			writeDockerError(w, errors.New("image runtime returned an invalid history size"))
			return
		}
		result = append(result, historyItem{
			ID: "<missing>", Created: parseDockerTime(item.CreatedAt),
			CreatedBy: item.CreatedBy, Tags: []string{}, Size: size, Comment: item.Comment,
		})
	}
	if len(result) > 0 {
		document, err := a.manager.inspect(r.Context(), "image", normalizeNerdctlReference(id))
		if err != nil {
			writeDockerError(w, err)
			return
		}
		var image struct {
			ID       string `json:"Id"`
			RepoTags []string
		}
		if err := json.Unmarshal(document, &image); err != nil {
			writeDockerError(w, fmt.Errorf("decode image history identity: %w", err))
			return
		}
		if image.ID == "" {
			writeDockerError(w, errors.New("image runtime returned no history image identity"))
			return
		}
		result[0].ID, result[0].Tags = image.ID, image.RepoTags
	}
	writeDockerJSON(w, http.StatusOK, result)
}
