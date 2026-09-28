package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type CommitContainerRequest struct {
	Container string
	Reference string
	Author    string
	Message   string
	Pause     bool
	Changes   []string
}

func (a *API) exportContainer(w http.ResponseWriter, r *http.Request) {
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartContainerExport(processContext, r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	var stderr bytes.Buffer
	var stderrDone sync.WaitGroup
	stderrDone.Add(1)
	go func() {
		defer stderrDone.Done()
		_, _ = io.Copy(&stderr, process.Stderr())
	}()
	_ = process.Stdin().Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	_, copyErr := io.Copy(w, process.Stdout())
	waitErr := process.Wait()
	stderrDone.Wait()
	if copyErr != nil {
		_ = process.Kill()
		return
	}
	if waitErr != nil {
		log.Printf(
			"export Porto container %s failed after streaming started: %v: %s",
			r.PathValue("id"),
			waitErr,
			strings.TrimSpace(stderr.String()),
		)
		return
	}
}

func (a *API) commitContainer(w http.ResponseWriter, r *http.Request) {
	reference, err := taggedImageReference(r.URL.Query().Get("repo"), r.URL.Query().Get("tag"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	pause := true
	if value := r.URL.Query().Get("pause"); value != "" {
		pause = dockerBool(r, "pause")
	}
	id, err := a.manager.CommitContainer(r.Context(), CommitContainerRequest{
		Container: r.URL.Query().Get("container"),
		Reference: reference,
		Author:    r.URL.Query().Get("author"),
		Message:   r.URL.Query().Get("comment"),
		Pause:     pause,
		Changes:   append([]string(nil), r.URL.Query()["changes"]...),
	})
	if err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusCreated, map[string]string{"Id": id})
}

func (m *Manager) StartContainerExport(ctx context.Context, id string) (runtimes.Process, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	return m.startProcess(ctx, "export Porto container", "container", "export", id)
}

func (m *Manager) CommitContainer(
	ctx context.Context,
	request CommitContainerRequest,
) (string, error) {
	if err := validateObjectID(request.Container); err != nil {
		return "", fmt.Errorf("container: %w", err)
	}
	if err := validateObjectID(request.Reference); err != nil {
		return "", fmt.Errorf("image reference: %w", err)
	}
	args := []string{"commit"}
	for flag, value := range map[string]string{
		"--author":  request.Author,
		"--message": request.Message,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", errors.New("invalid container commit metadata")
		}
		args = appendStringFlag(args, flag, value)
	}
	args = append(args, fmt.Sprintf("--pause=%t", request.Pause))
	for _, change := range request.Changes {
		if strings.TrimSpace(change) == "" || strings.ContainsAny(change, "\r\n\x00") {
			return "", errors.New("invalid container commit change")
		}
		args = append(args, "--change", change)
	}
	args = append(args, request.Container, normalizeNerdctlReference(request.Reference))
	output, err := m.run(ctx, "commit Porto container", args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(output))
	if id == "" {
		return "", errors.New("container runtime returned an empty committed image identifier")
	}
	return id, nil
}
