//go:build linux

package runtimefiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/opencontainers/image-spec/identity"
	"golang.org/x/sys/unix"
)

func withDirectory(ctx context.Context, descriptor Descriptor, readOnly bool, run func(Descriptor) error) (err error) {
	if descriptor.Resource.Kind == "volume" {
		return run(descriptor)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	directory, err := os.MkdirTemp("", "porto-files-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(directory)) }()
	// Mount only inside this helper's private namespace. Concurrent helpers and
	// workload mount namespaces are never modified by inspector operations.
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("%w: snapshot access requires Linux mount permission (run the packaged helper through sudo -n): %w", datafiles.ErrUnsupported, err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return err
	}
	prepared, cleanup, err := prepare(ctx, descriptor, directory, readOnly)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	return run(prepared)
}

func prepare(ctx context.Context, descriptor Descriptor, directory string, readOnly bool) (Descriptor, func() error, error) {
	client, err := runtimeClient(descriptor)
	if err != nil {
		return descriptor, nil, err
	}
	ctx = namespaces.WithNamespace(ctx, descriptor.Namespace)
	var createdSnapshot string
	snapshotter := descriptor.Snapshotter
	if snapshotter == "" {
		snapshotter = "overlayfs"
	}
	cleanup := func() error {
		unmountErr := mount.UnmountAll(directory, 0)
		if unmountErr != nil {
			return errors.Join(unmountErr, client.Close())
		}
		var removeErr error
		if createdSnapshot != "" {
			cleanupContext, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), descriptor.Namespace), 20*time.Second)
			defer cancel()
			removeErr = client.SnapshotService(snapshotter).Remove(cleanupContext, createdSnapshot)
		}
		return errors.Join(removeErr, client.Close())
	}
	fail := func(err error) (Descriptor, func() error, error) {
		return descriptor, nil, errors.Join(err, cleanup())
	}
	switch descriptor.Resource.Kind {
	case "container":
		container, err := client.LoadContainer(ctx, descriptor.Resource.ID)
		if err != nil {
			return fail(err)
		}
		info, err := container.Info(ctx)
		if err != nil || info.CreatedAt.UTC().Format(time.RFC3339Nano) != descriptor.Resource.CreatedAt ||
			info.SnapshotKey != descriptor.SnapshotKey || info.Snapshotter != descriptor.Snapshotter {
			return fail(errors.Join(datafiles.ErrConflict, err))
		}
		spec, err := container.Spec(ctx)
		if err != nil || spec.Root == nil {
			return fail(errors.Join(datafiles.ErrUnsupported, err))
		}
		if spec.Root.Readonly && !readOnly {
			return fail(fmt.Errorf("%w: container rootfs is read-only", datafiles.ErrUnsupported))
		}
		task, taskErr := container.Task(ctx, nil)
		running := false
		if taskErr == nil {
			state, err := task.Status(ctx)
			if err != nil {
				return fail(err)
			}
			running = state.Status == containerd.Running || state.Status == containerd.Paused
			if running {
				if descriptor.PID != task.Pid() {
					return fail(datafiles.ErrConflict)
				}
				if err := unix.Mount(fmt.Sprintf("/proc/%d/root", task.Pid()), directory, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
					return fail(err)
				}
			}
		} else if !errdefs.IsNotFound(taskErr) {
			return fail(taskErr)
		}
		if !running {
			if descriptor.PID != 0 {
				return fail(datafiles.ErrConflict)
			}
			mounts, err := client.SnapshotService(snapshotter).Mounts(ctx, info.SnapshotKey)
			if err != nil {
				return fail(err)
			}
			if err := mount.All(mounts, directory); err != nil {
				return fail(err)
			}
			if err := bindStoppedMounts(directory, descriptor); err != nil {
				return fail(err)
			}
		}
	case "image":
		if !readOnly {
			return fail(fmt.Errorf("%w: immutable images cannot be writable", datafiles.ErrUnsupported))
		}
		image, err := client.GetImage(ctx, descriptor.Resource.Name)
		if err != nil || image.Target().Digest.String() != descriptor.Resource.ID {
			return fail(errors.Join(datafiles.ErrConflict, err))
		}
		if descriptor.Platform != "" {
			platform, err := platforms.Parse(descriptor.Platform)
			if err != nil || platform.OS != "linux" {
				return fail(errors.Join(datafiles.ErrUnsupported, err))
			}
			image = containerd.NewImageWithPlatform(client, image.Metadata(), platforms.OnlyStrict(platform))
		}
		unpacked, err := image.IsUnpacked(ctx, snapshotter)
		if err != nil {
			return fail(err)
		}
		if !unpacked {
			if err := image.Unpack(ctx, snapshotter); err != nil {
				return fail(err)
			}
		}
		layers, err := image.RootFS(ctx)
		if err != nil {
			return fail(err)
		}
		createdSnapshot = "porto-files-" + filepath.Base(directory)
		mounts, err := client.SnapshotService(snapshotter).View(ctx, createdSnapshot, identity.ChainID(layers).String(),
			snapshots.WithLabels(map[string]string{
				"porto.files.owner": descriptor.Owner, "porto.files.identity": descriptor.Resource.Fingerprint(),
				"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339Nano),
			}))
		if err != nil {
			createdSnapshot = ""
			return fail(err)
		}
		if err := mount.All(mounts, directory); err != nil {
			return fail(err)
		}
		descriptor.Resource.ReadOnly = true
	case "volume":
		info, err := os.Lstat(descriptor.RootPath)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fail(errors.Join(datafiles.ErrConflict, err))
		}
		if err := unix.Mount(descriptor.RootPath, directory, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fail(err)
		}
	default:
		return fail(datafiles.ErrUnsupported)
	}
	attributes := uint64(unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC | unix.MOUNT_ATTR_NOSYMFOLLOW)
	if readOnly || descriptor.Resource.ReadOnly {
		attributes |= unix.MOUNT_ATTR_RDONLY
	}
	if err := unix.MountSetattr(unix.AT_FDCWD, directory, unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: attributes}); err != nil {
		return fail(fmt.Errorf("%w: safe recursive mount attributes require Linux 5.12 or newer: %w", datafiles.ErrUnsupported, err))
	}
	descriptor.RootPath = directory
	descriptor.Snapshotter = snapshotter
	if createdSnapshot != "" {
		descriptor.SnapshotKey = createdSnapshot
	}
	return descriptor, cleanup, nil
}

func bindStoppedMounts(directory string, descriptor Descriptor) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, mounted := range descriptor.Mounts {
		if mounted.Type != "bind" || mounted.Source == "" {
			continue
		}
		relative := strings.TrimPrefix(filepath.Clean(mounted.Destination), "/")
		if err := datafiles.ValidatePath(relative); err != nil {
			return err
		}
		source, err := os.Stat(mounted.Source)
		if err != nil {
			return fmt.Errorf("stopped-container mount %s is unavailable: %w", mounted.Destination, err)
		}
		if source.IsDir() {
			if err := root.MkdirAll(relative, 0o755); err != nil {
				return err
			}
		} else {
			file, err := root.OpenFile(relative, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
		}
		target, err := root.Open(relative)
		if err != nil {
			return err
		}
		mountPath := fmt.Sprintf("/proc/self/fd/%d", target.Fd())
		mountErr := unix.Mount(mounted.Source, mountPath, "", unix.MS_BIND|unix.MS_REC, "")
		if mountErr == nil && slices.Contains(mounted.Options, "ro") {
			mountErr = unix.MountSetattr(unix.AT_FDCWD, mountPath, unix.AT_RECURSIVE, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
		}
		if err := errors.Join(mountErr, target.Close()); err != nil {
			return err
		}
	}
	return nil
}

func runtimeClient(descriptor Descriptor) (*containerd.Client, error) {
	if !filepath.IsAbs(descriptor.Address) || descriptor.Namespace == "" ||
		strings.ContainsAny(descriptor.Address+descriptor.Namespace, "\r\n\x00") {
		return nil, fmt.Errorf("%w: containerd address and namespace are required", datafiles.ErrInvalid)
	}
	return containerd.New(descriptor.Address, containerd.WithDefaultNamespace(descriptor.Namespace))
}

func ensureVolumeIdle(ctx context.Context, descriptor Descriptor) error {
	client, err := runtimeClient(descriptor)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx = namespaces.WithNamespace(ctx, descriptor.Namespace)
	containers, err := client.Containers(ctx)
	if err != nil {
		return err
	}
	for _, container := range containers {
		spec, err := container.Spec(ctx)
		if err != nil {
			return err
		}
		references := false
		for _, mounted := range spec.Mounts {
			if mounted.Source == descriptor.RootPath || strings.HasPrefix(mounted.Source, strings.TrimSuffix(descriptor.RootPath, "/")+"/") {
				references = true
				break
			}
		}
		if !references {
			continue
		}
		task, err := container.Task(ctx, nil)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		state, err := task.Status(ctx)
		if err != nil {
			return err
		}
		if state.Status != containerd.Stopped {
			return fmt.Errorf("%w: volume is used by active container %s; stop its writers first", datafiles.ErrConflict, container.ID())
		}
	}
	return nil
}

func nativeDirectory(descriptor Descriptor) (string, error) {
	if descriptor.Owner == "" || strings.ContainsAny(descriptor.Owner, "/\\\x00\r\n") {
		return "", fmt.Errorf("%w: native attachment ownership is required", datafiles.ErrInvalid)
	}
	directory := filepath.Join("/run/porto-native", descriptor.Owner)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(datafiles.ErrConflict, err)
	}
	return directory, nil
}

func attach(ctx context.Context, descriptor Descriptor, writable bool) (attachment Attachment, err error) {
	if writable && (descriptor.Resource.ReadOnly || descriptor.Resource.Kind == "image") {
		return attachment, fmt.Errorf("%w: immutable or read-only resources cannot be attached writable", datafiles.ErrUnsupported)
	}
	base, err := nativeDirectory(descriptor)
	if err != nil {
		return attachment, err
	}
	token := descriptor.Resource.Fingerprint() + "-ro"
	if writable {
		token = descriptor.Resource.Fingerprint() + "-rw"
	}
	directory := filepath.Join(base, token)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return attachment, fmt.Errorf("%w: attachment already exists; detach it before retrying: %w", datafiles.ErrConflict, err)
	}
	prepared, cleanup, err := prepare(ctx, descriptor, directory, !writable)
	if err != nil {
		return attachment, errors.Join(err, os.Remove(directory))
	}
	attachment = Attachment{
		Token: token, Path: directory, Resource: descriptor.Resource, Identity: descriptor.Resource.Fingerprint(),
		ReadOnly: !writable, Snapshotter: prepared.Snapshotter,
	}
	if descriptor.Resource.Kind == "image" {
		attachment.SnapshotKey = prepared.SnapshotKey
	}
	document, err := json.Marshal(attachment)
	if err == nil {
		err = os.WriteFile(directory+".json", document, 0o600)
	}
	if err != nil {
		return attachment, errors.Join(err, cleanup(), os.Remove(directory))
	}
	return attachment, nil
}

func detach(ctx context.Context, descriptor Descriptor, token string) error {
	if token != descriptor.Resource.Fingerprint()+"-ro" && token != descriptor.Resource.Fingerprint()+"-rw" {
		return datafiles.ErrConflict
	}
	base, err := nativeDirectory(descriptor)
	if err != nil {
		return err
	}
	directory := filepath.Join(base, token)
	document, err := os.ReadFile(directory + ".json")
	if err != nil {
		return err
	}
	var attachment Attachment
	if err := json.Unmarshal(document, &attachment); err != nil {
		return err
	}
	if attachment.Identity != descriptor.Resource.Fingerprint() || attachment.Path != directory || attachment.Token != token {
		return datafiles.ErrConflict
	}
	if err := mount.UnmountAll(directory, 0); err != nil {
		return fmt.Errorf("detach native filesystem; source data was not removed: %w", err)
	}
	if attachment.SnapshotKey != "" {
		client, err := runtimeClient(descriptor)
		if err != nil {
			return err
		}
		removeErr := client.SnapshotService(attachment.Snapshotter).Remove(namespaces.WithNamespace(ctx, descriptor.Namespace), attachment.SnapshotKey)
		if err := errors.Join(removeErr, client.Close()); err != nil {
			return err
		}
	}
	return errors.Join(os.Remove(directory), os.Remove(directory+".json"))
}
