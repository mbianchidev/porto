package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

type containerConsoleOperations interface {
	ConfigureConsole(context.Context, string, [2]uint) error
	ResizeConsole(context.Context, string, uint32, uint32) error
}

func (m *Manager) configureContainerConsole(ctx context.Context, id string, size [2]uint) error {
	if m.IsNativeWindows() {
		return m.withWindowsConsole(ctx, func(console containerConsoleOperations) error { return console.ConfigureConsole(ctx, id, size) })
	}
	handled, err, reason := m.attemptContainerOperation(ctx, func(operations containerOperations) error {
		console, ok := operations.(containerConsoleOperations)
		if !ok {
			if borrowed, ok := operations.(borrowedContainerOperations); ok {
				console, _ = borrowed.containerOperations.(containerConsoleOperations)
			}
		}
		if console == nil {
			return fmt.Errorf("%w: initial container console size", ErrUnsupported)
		}
		return console.ConfigureConsole(ctx, id, size)
	})
	if !handled {
		if reason != nil {
			return reason
		}
		return fmt.Errorf("%w: initial container console size requires containerd", ErrUnsupported)
	}
	return err
}

func (a *API) resizeContainer(w http.ResponseWriter, r *http.Request) {
	width, widthErr := strconv.ParseUint(r.URL.Query().Get("w"), 10, 32)
	height, heightErr := strconv.ParseUint(r.URL.Query().Get("h"), 10, 32)
	if widthErr != nil || heightErr != nil || width == 0 || height == 0 {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "positive terminal width and height are required"})
		return
	}
	if a.manager.IsNativeWindows() {
		if err := a.manager.resizeWindowsConsole(r.Context(), r.PathValue("id"), uint32(width), uint32(height)); err != nil {
			writeDockerError(w, err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	handled, err, reason := a.manager.attemptContainerOperation(r.Context(), func(operations containerOperations) error {
		console, ok := operations.(containerConsoleOperations)
		if !ok {
			if borrowed, ok := operations.(borrowedContainerOperations); ok {
				console, _ = borrowed.containerOperations.(containerConsoleOperations)
			}
		}
		if console == nil {
			return fmt.Errorf("%w: container terminal resize", ErrUnsupported)
		}
		return console.ResizeConsole(r.Context(), r.PathValue("id"), uint32(width), uint32(height))
	})
	if !handled && err == nil {
		err = reason
		if err == nil {
			err = fmt.Errorf("%w: container terminal resize requires containerd", ErrUnsupported)
		}
	}
	if err != nil {
		writeDockerError(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *grpcContainerRuntime) ConfigureConsole(ctx context.Context, id string, size [2]uint) error {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	ctx = withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(ctx, &containersapi.GetContainerRequest{ID: id})
	if err != nil {
		return containerdOperationError("read terminal configuration for", id, err)
	}
	record := response.GetContainer()
	var spec specs.Spec
	if record.GetSpec() == nil {
		return fmt.Errorf("%w: container has no OCI terminal configuration", ErrUnsupported)
	}
	if err := json.Unmarshal(record.Spec.Value, &spec); err != nil {
		return fmt.Errorf("decode container terminal configuration: %w", err)
	}
	if spec.Process == nil || !spec.Process.Terminal {
		return fmt.Errorf("container %q does not use a TTY", id)
	}
	spec.Process.ConsoleSize = &specs.Box{Height: size[0], Width: size[1]}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	updated := proto.Clone(record).(*containersapi.Container)
	updated.Spec.Value = encoded
	if updated.Labels == nil {
		updated.Labels = make(map[string]string)
	}
	dimensions, err := json.Marshal(size)
	if err != nil {
		return err
	}
	updated.Labels["io.porto.container.console-size"] = string(dimensions)
	_, err = r.containers.Update(ctx, &containersapi.UpdateContainerRequest{
		Container: updated, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"spec", "labels.io.porto.container.console-size"}},
	})
	return containerdOperationError("configure terminal for", id, err)
}

func (r *grpcContainerRuntime) ResizeConsole(ctx context.Context, id string, width, height uint32) error {
	id, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	ctx = withContainerdNamespace(ctx, r.namespace)
	response, err := r.containers.Get(ctx, &containersapi.GetContainerRequest{ID: id})
	if err != nil {
		return containerdOperationError("read terminal configuration for", id, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(response.GetContainer().GetSpec().GetValue(), &spec); err != nil {
		return fmt.Errorf("decode container terminal configuration: %w", err)
	}
	if spec.Process == nil || !spec.Process.Terminal {
		return fmt.Errorf("container %q does not use a TTY", id)
	}
	_, err = r.tasks.ResizePty(ctx, &tasksapi.ResizePtyRequest{ContainerID: id, Width: width, Height: height})
	return containerdOperationError("resize terminal for", id, err)
}
