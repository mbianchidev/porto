//go:build linux

package runtimefiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/mbianchidev/porto/internal/datafiles"
	"golang.org/x/sys/unix"
)

func containerChanges(ctx context.Context, descriptor Descriptor, output io.Writer) (err error) {
	if descriptor.Resource.Kind != "container" || descriptor.SnapshotKey == "" || descriptor.RootPath != "" {
		return fmt.Errorf("%w: filesystem changes require a container snapshot", datafiles.ErrInvalid)
	}
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
	if err != nil || info.SnapshotKey != descriptor.SnapshotKey || info.Snapshotter != descriptor.Snapshotter ||
		info.CreatedAt.UTC().Format(time.RFC3339Nano) != descriptor.Resource.CreatedAt {
		return errors.Join(datafiles.ErrConflict, err)
	}
	store := client.SnapshotService(info.Snapshotter)
	snapshot, err := store.Stat(ctx, info.SnapshotKey)
	if err != nil {
		return err
	}
	upper, err := store.Mounts(ctx, info.SnapshotKey)
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("%w: filesystem changes require an isolated Linux mount namespace: %w", datafiles.ErrUnsupported, err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	directory, err := os.MkdirTemp("", "porto-diff-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(directory)) }()
	var lower []mount.Mount
	if snapshot.Parent != "" {
		key := "porto-diff-" + filepath.Base(directory)
		lower, err = store.View(ctx, key, snapshot.Parent, snapshots.WithLabels(map[string]string{
			"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339Nano),
		}))
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), descriptor.Namespace), 20*time.Second)
			defer cancel()
			err = errors.Join(err, store.Remove(cleanup, key))
		}()
	}
	compare := func(lowerDirectory string) error {
		return mount.WithReadonlyTempMount(ctx, upper, func(upperDirectory string) error {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			// The diff walker starts goroutines. A child makes every Go thread
			// inherit this private mount namespace, not just the locked caller.
			command := exec.CommandContext(ctx, executable, "filesystem-changes", lowerDirectory, upperDirectory)
			command.Stdout = output
			var diagnostics boundedNamespaceDiagnostics
			command.Stderr = &diagnostics
			if err := command.Run(); err != nil {
				return fmt.Errorf("compare container snapshots: %w: %s", err, diagnostics.String())
			}
			return nil
		})
	}
	if len(lower) > 0 {
		err = mount.WithReadonlyTempMount(ctx, lower, compare)
	} else {
		err = compare(directory)
	}
	if err != nil {
		return err
	}
	return nil
}
