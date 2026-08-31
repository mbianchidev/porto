package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sync"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const maxContainerArchiveBytes = int64(2 * 1024 * 1024 * 1024)

func (a *API) containerArchive(w http.ResponseWriter, r *http.Request) {
	containerPath := r.URL.Query().Get("path")
	stat, err := a.manager.ContainerPathStat(r.Context(), r.PathValue("id"), containerPath)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if err := setContainerPathStat(w.Header(), stat); err != nil {
		writeDockerError(w, err)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartArchiveDownload(processContext, r.PathValue("id"), containerPath)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	var stderr bytes.Buffer
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		if _, err := io.Copy(w, process.Stdout()); err != nil {
			_ = process.Kill()
			_, _ = io.Copy(io.Discard, process.Stdout())
		}
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(&stderr, process.Stderr())
	}()
	_ = process.Stdin().Close()
	wait.Wait()
	waitErr := process.Wait()
	if waitErr != nil {
		return
	}
}

func (a *API) putContainerArchive(w http.ResponseWriter, r *http.Request) {
	containerPath := r.URL.Query().Get("path")
	stat, err := a.manager.ContainerPathStat(r.Context(), r.PathValue("id"), containerPath)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if !stat.Mode.IsDir() {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "container archive destination is not a directory"})
		return
	}
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartArchiveUpload(
		processContext,
		r.PathValue("id"),
		containerPath,
		dockerBool(r, "copyUIDGID"),
	)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(&stdout, process.Stdout())
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(&stderr, process.Stderr())
	}()
	_, copyErr := io.Copy(process.Stdin(), http.MaxBytesReader(w, r.Body, maxContainerArchiveBytes))
	closeErr := process.Stdin().Close()
	wait.Wait()
	waitErr := process.Wait()
	if copyErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(copyErr, &tooLarge) {
			writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "container archive exceeds the 2 GiB limit"})
			return
		}
		writeDockerError(w, fmt.Errorf("stream container archive: %w", copyErr))
		return
	}
	if closeErr != nil {
		writeDockerError(w, fmt.Errorf("close container archive input: %w", closeErr))
		return
	}
	if waitErr != nil {
		writeDockerError(w, processCommandError("copy archive into Porto container", stderr.Bytes(), waitErr))
		return
	}
	w.WriteHeader(http.StatusOK)
}

func setContainerPathStat(header http.Header, stat PathStat) error {
	encoded, err := json.Marshal(stat)
	if err != nil {
		return fmt.Errorf("encode container path stat: %w", err)
	}
	header.Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(encoded))
	return nil
}

func processCommandError(action string, stderr []byte, err error) error {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return runtimes.CommandError(action, stderr, exitError)
	}
	return fmt.Errorf("%s: %w", action, err)
}
