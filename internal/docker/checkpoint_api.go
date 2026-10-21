package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
)

var checkpointNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type namedCheckpointOperations interface {
	CreateNamedCheckpoint(context.Context, string, string, bool) error
	ListNamedCheckpoints(context.Context, string) ([]string, error)
	DeleteNamedCheckpoint(context.Context, string, string) error
}

func (a *API) dockerCheckpointOperation(w http.ResponseWriter, r *http.Request, operation func(namedCheckpointOperations) error) bool {
	if r.URL.Query().Get("dir") != "" {
		writeDockerUnsupported(w, "custom checkpoint directories; checkpoints are stored as containerd images")
		return false
	}
	handled, err, reason := a.manager.attemptContainerOperation(r.Context(), func(operations containerOperations) error {
		if borrowed, ok := operations.(borrowedContainerOperations); ok {
			operations = borrowed.containerOperations
		}
		named, ok := operations.(namedCheckpointOperations)
		if !ok {
			return fmt.Errorf("%w: named container checkpoints", ErrUnsupported)
		}
		return operation(named)
	})
	if !handled && err == nil {
		err = reason
		if err == nil {
			err = fmt.Errorf("%w: named container checkpoints require containerd", ErrUnsupported)
		}
	}
	if err != nil {
		writeDockerError(w, err)
		return false
	}
	return true
}

func (a *API) dockerCreateCheckpoint(w http.ResponseWriter, r *http.Request) {
	var request struct {
		CheckpointID  string `json:"CheckpointID"`
		CheckpointDir string `json:"CheckpointDir"`
		Exit          bool   `json:"Exit"`
	}
	if !decodeDockerOptionsJSON(w, r, &request) {
		return
	}
	if !checkpointNamePattern.MatchString(request.CheckpointID) {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid checkpoint name"})
		return
	}
	if request.CheckpointDir != "" {
		writeDockerUnsupported(w, "custom checkpoint directories; checkpoints are stored as containerd images")
		return
	}
	if a.dockerCheckpointOperation(w, r, func(operations namedCheckpointOperations) error {
		return operations.CreateNamedCheckpoint(r.Context(), r.PathValue("id"), request.CheckpointID, request.Exit)
	}) {
		a.manager.invalidateContainerInventory()
		w.WriteHeader(http.StatusCreated)
	}
}

func (a *API) dockerListCheckpoints(w http.ResponseWriter, r *http.Request) {
	var names []string
	if !a.dockerCheckpointOperation(w, r, func(operations namedCheckpointOperations) error {
		var err error
		names, err = operations.ListNamedCheckpoints(r.Context(), r.PathValue("id"))
		return err
	}) {
		return
	}
	result := make([]struct{ Name string }, 0, len(names))
	for _, name := range names {
		result = append(result, struct{ Name string }{name})
	}
	writeDockerJSON(w, http.StatusOK, result)
}

func (a *API) dockerDeleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("checkpoint")
	if !checkpointNamePattern.MatchString(name) {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid checkpoint name"})
		return
	}
	if a.dockerCheckpointOperation(w, r, func(operations namedCheckpointOperations) error {
		return operations.DeleteNamedCheckpoint(r.Context(), r.PathValue("id"), name)
	}) {
		w.WriteHeader(http.StatusNoContent)
	}
}

func namedCheckpointReference(id, name string) string {
	return "porto.local/checkpoints/" + id + ":" + name
}

func (r *grpcContainerRuntime) CreateNamedCheckpoint(ctx context.Context, id, name string, exit bool) error {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	if !checkpointNamePattern.MatchString(name) {
		return fmt.Errorf("invalid checkpoint name")
	}
	if r.client == nil {
		return fmt.Errorf("%w: checkpoint image store", ErrUnavailable)
	}
	capability := r.Capabilities(ctx).CheckpointRestore
	if !capability.Supported {
		return fmt.Errorf("%w: %s", ErrUnsupported, capability.Reason)
	}
	unlock := r.networkLocks.lock(id)
	defer unlock()
	reference := namedCheckpointReference(id, name)
	if _, err := r.client.GetImage(withContainerdNamespace(ctx, r.namespace), reference); err == nil {
		return fmt.Errorf("%w: checkpoint %q already exists", ErrConflict, name)
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("check checkpoint identity: %w", err)
	}
	var options []containerd.CheckpointOpts
	var originalRestart map[string]string
	if exit {
		response, err := r.containers.Get(withContainerdNamespace(ctx, r.namespace), &containersapi.GetContainerRequest{ID: id})
		if err != nil {
			return containerdOperationError("read checkpoint restart state for", id, err)
		}
		labels := response.GetContainer().GetLabels()
		originalRestart = map[string]string{
			restartStatusLabel:  labels[restartStatusLabel],
			restartStoppedLabel: labels[restartStoppedLabel],
		}
		if err := r.setRestartDesired(ctx, id, false); err != nil {
			return err
		}
		options = append(options, containerd.WithCheckpointTaskExit)
	}
	_, err = r.checkpointContainer(ctx, id, reference, options...)
	if err != nil && originalRestart != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		err = errors.Join(err, r.updateLabelsResolved(cleanup, id, originalRestart))
	}
	return err
}

func (r *grpcContainerRuntime) ListNamedCheckpoints(ctx context.Context, id string) ([]string, error) {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.client == nil {
		return nil, fmt.Errorf("%w: checkpoint image store", ErrUnavailable)
	}
	images, err := r.client.ImageService().List(withContainerdNamespace(ctx, r.namespace))
	if err != nil {
		return nil, fmt.Errorf("list checkpoint images: %w", err)
	}
	names := make([]string, 0)
	prefix := namedCheckpointReference(id, "")
	for _, image := range images {
		if image.Labels[checkpointSourceIDLabel] == id && strings.HasPrefix(image.Name, prefix) {
			names = append(names, strings.TrimPrefix(image.Name, prefix))
		}
	}
	slices.Sort(names)
	return names, nil
}

func (r *grpcContainerRuntime) DeleteNamedCheckpoint(ctx context.Context, id, name string) error {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	if !checkpointNamePattern.MatchString(name) {
		return fmt.Errorf("invalid checkpoint name")
	}
	if r.client == nil {
		return fmt.Errorf("%w: checkpoint image store", ErrUnavailable)
	}
	ctx = withContainerdNamespace(ctx, r.namespace)
	reference := namedCheckpointReference(id, name)
	image, err := r.client.GetImage(ctx, reference)
	if err != nil {
		return containerdOperationError("find checkpoint "+name+" for", id, err)
	}
	if image.Labels()[checkpointSourceIDLabel] != id {
		return fmt.Errorf("%w: checkpoint source does not match container", ErrConflict)
	}
	return r.client.ImageService().Delete(ctx, reference)
}
