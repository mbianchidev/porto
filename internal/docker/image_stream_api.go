package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

func (a *API) getImages(w http.ResponseWriter, r *http.Request) {
	images := append([]string(nil), r.URL.Query()["names"]...)
	if len(images) == 0 {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "at least one image name is required"})
		return
	}
	processContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	process, err := a.manager.StartImageSave(processContext, images)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	archive, err := os.CreateTemp("", "porto-images-*.tar")
	if err != nil {
		_ = process.Kill()
		writeDockerError(w, fmt.Errorf("create image archive: %w", err))
		return
	}
	archivePath := archive.Name()
	defer func() {
		_ = archive.Close()
		_ = os.Remove(archivePath)
	}()
	var stderr bytes.Buffer
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, _ = io.Copy(archive, process.Stdout())
	}()
	go func() {
		defer wait.Done()
		_, _ = io.Copy(&stderr, process.Stderr())
	}()
	_ = process.Stdin().Close()
	wait.Wait()
	waitErr := process.Wait()
	if waitErr != nil {
		writeDockerError(w, processCommandError("save Porto images", stderr.Bytes(), waitErr))
		return
	}
	servedArchive, err := platformImageArchive(archive)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	if servedArchive != archive {
		defer func() {
			_ = servedArchive.Close()
			_ = os.Remove(servedArchive.Name())
		}()
	}
	info, err := servedArchive.Stat()
	if err != nil {
		writeDockerError(w, fmt.Errorf("inspect image archive: %w", err))
		return
	}
	if info.Size() == 0 {
		writeDockerError(w, errors.New("container runtime returned an empty image archive"))
		return
	}
	if _, err := servedArchive.Seek(0, io.SeekStart); err != nil {
		writeDockerError(w, fmt.Errorf("rewind image archive: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, servedArchive)
}
