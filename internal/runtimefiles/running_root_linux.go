//go:build linux

package runtimefiles

import (
	"context"
	"errors"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mbianchidev/porto/internal/datafiles"
)

func withRunningContainerRoot(ctx context.Context, descriptor Descriptor, readOnly bool, run func(Descriptor) error) (err error) {
	client, err := runtimeClient(descriptor)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	ctx = namespaces.WithNamespace(ctx, descriptor.Namespace)
	container, err := client.LoadContainer(ctx, descriptor.Resource.ID)
	if err != nil {
		return err
	}
	info, err := container.Info(ctx)
	if err != nil || info.CreatedAt.UTC().Format(time.RFC3339Nano) != descriptor.Resource.CreatedAt ||
		info.SnapshotKey != descriptor.SnapshotKey || info.Snapshotter != descriptor.Snapshotter {
		return errors.Join(datafiles.ErrConflict, err)
	}
	spec, err := container.Spec(ctx)
	if err != nil || spec.Root == nil {
		return errors.Join(datafiles.ErrUnsupported, err)
	}
	if !readOnly && spec.Root.Readonly {
		return fmt.Errorf("%w: container rootfs is read-only", datafiles.ErrUnsupported)
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return err
	}
	state, err := task.Status(ctx)
	if err != nil || task.Pid() != descriptor.PID || (state.Status != containerd.Running && state.Status != containerd.Paused) {
		return errors.Join(datafiles.ErrConflict, err)
	}
	// OpenRoot uses the live mount namespace directly. Binding another user
	// namespace's locked /proc subtree is rejected by rootless kernels.
	descriptor.RootPath = fmt.Sprintf("/proc/%d/root", descriptor.PID)
	return run(descriptor)
}
