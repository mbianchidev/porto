package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
)

func selectDockerContainers(containers []Container, filters dockerFilters, since, before string) ([]Container, error) {
	result := slices.Clone(containers)
	slices.SortStableFunc(result, func(left, right Container) int {
		return parseDockerTimestamp(right.CreatedAt).Compare(parseDockerTimestamp(left.CreatedAt))
	})
	for _, boundary := range []struct {
		key   string
		value string
	}{{"since", since}, {"before", before}} {
		values := make(map[string]bool)
		for value, enabled := range filters[boundary.key] {
			if enabled {
				values[value] = true
			}
		}
		if boundary.value != "" {
			values[boundary.value] = true
		}
		for value := range values {
			match := -1
			for index, container := range containers {
				if container.ID == value || strings.TrimPrefix(container.Name, "/") == strings.TrimPrefix(value, "/") {
					match = index
					break
				}
			}
			if match < 0 {
				for index, container := range containers {
					if strings.HasPrefix(container.ID, value) {
						if match >= 0 {
							return nil, fmt.Errorf("%w: ambiguous container %q", ErrConflict, value)
						}
						match = index
					}
				}
			}
			if match < 0 {
				return nil, fmt.Errorf("%w: no such container %q", ErrNotFound, value)
			}
			created := parseDockerTimestamp(containers[match].CreatedAt)
			result = slices.DeleteFunc(result, func(container Container) bool {
				if boundary.key == "since" {
					return !parseDockerTimestamp(container.CreatedAt).After(created)
				}
				return !parseDockerTimestamp(container.CreatedAt).Before(created)
			})
		}
	}
	return result, nil
}

func (a *API) containerTop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validateObjectID(id); err != nil {
		writeDockerError(w, err)
		return
	}
	args := []string{"top", id}
	if value := r.URL.Query().Get("ps_args"); value != "" {
		if strings.ContainsAny(value, "\r\n\x00") {
			writeDockerError(w, errors.New("invalid process-list arguments"))
			return
		}
		args = append(args, strings.Fields(value)...)
	}
	output, err := a.manager.run(r.Context(), "list container processes", args...)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	titles := strings.Fields(lines[0])
	if len(titles) == 0 {
		writeDockerError(w, errors.New("container runtime returned no process-list titles"))
		return
	}
	processes := make([][]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < len(titles) {
			writeDockerError(w, errors.New("container runtime returned a malformed process list"))
			return
		}
		row := append([]string(nil), fields[:len(titles)-1]...)
		row = append(row, strings.Join(fields[len(titles)-1:], " "))
		processes = append(processes, row)
	}
	writeDockerJSON(w, http.StatusOK, struct {
		Titles    []string
		Processes [][]string
	}{titles, processes})
}

func (a *API) containerChanges(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validateObjectID(id); err != nil {
		writeDockerError(w, err)
		return
	}
	if !a.manager.directCLI {
		descriptor, err := a.manager.FileDescriptor(r.Context(), "container", id)
		if err != nil {
			writeDockerError(w, err)
			return
		}
		var output bytes.Buffer
		if err := a.manager.RunFileRequest(r.Context(), descriptor, datafiles.Request{
			Action: "changes", Identity: descriptor.Resource.Fingerprint(),
		}, nil, &output); err != nil {
			writeDockerError(w, err)
			return
		}
		var changes []runtimefiles.FilesystemChange
		if err := json.Unmarshal(output.Bytes(), &changes); err != nil {
			writeDockerError(w, fmt.Errorf("decode container filesystem changes: %w", err))
			return
		}
		writeDockerJSON(w, http.StatusOK, changes)
		return
	}
	output, err := a.manager.run(r.Context(), "read container filesystem changes", "diff", id)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	changes := make([]struct {
		Path string
		Kind int
	}, 0)
	for _, line := range strings.Split(strings.TrimSuffix(string(output), "\n"), "\n") {
		if line == "" {
			continue
		}
		kind, valid := map[byte]int{'C': 0, 'A': 1, 'D': 2}[line[0]]
		if !valid || len(line) < 3 || line[1] != ' ' || line[2] != '/' {
			writeDockerError(w, errors.New("container runtime returned a malformed filesystem change"))
			return
		}
		changes = append(changes, struct {
			Path string
			Kind int
		}{line[2:], kind})
	}
	writeDockerJSON(w, http.StatusOK, changes)
}

type containerSizeOperations interface {
	DiskUsage(context.Context, string) (int64, int64, error)
}

func (m *Manager) containerDiskUsage(ctx context.Context, id string) (rw, root int64, err error) {
	handled, err, reason := m.attemptContainerOperation(ctx, func(operations containerOperations) error {
		if borrowed, ok := operations.(borrowedContainerOperations); ok {
			operations = borrowed.containerOperations
		}
		provider, ok := operations.(containerSizeOperations)
		if !ok {
			return fmt.Errorf("%w: container snapshot size", ErrUnsupported)
		}
		rw, root, err = provider.DiskUsage(ctx, id)
		return err
	})
	if !handled && err == nil {
		err = reason
		if err == nil {
			err = fmt.Errorf("%w: container snapshot size requires containerd", ErrUnsupported)
		}
	}
	return rw, root, err
}

func (r *grpcContainerRuntime) DiskUsage(ctx context.Context, id string) (int64, int64, error) {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	if r.client == nil {
		return 0, 0, fmt.Errorf("%w: container snapshot client", ErrUnavailable)
	}
	ctx = withContainerdNamespace(ctx, r.namespace)
	container, err := r.client.LoadContainer(ctx, id)
	if err != nil {
		return 0, 0, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return 0, 0, err
	}
	if info.SnapshotKey == "" {
		return 0, 0, fmt.Errorf("%w: container has no writable snapshot", ErrUnsupported)
	}
	store := r.client.SnapshotService(info.Snapshotter)
	key := info.SnapshotKey
	var rw, root int64
	for key != "" {
		usage, err := store.Usage(ctx, key)
		if err != nil {
			return 0, 0, fmt.Errorf("measure container snapshot %q: %w", key, err)
		}
		if key == info.SnapshotKey {
			rw = usage.Size
		}
		root += usage.Size
		snapshot, err := store.Stat(ctx, key)
		if err != nil {
			return 0, 0, err
		}
		key = snapshot.Parent
	}
	return rw, root, nil
}
