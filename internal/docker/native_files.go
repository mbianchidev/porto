package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/nativefiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
)

func (m *Manager) SetNativeFilesGuard(guard func(string, string) error) { m.nativeGuard = guard }

func (m *Manager) SetNativeFilesClose(close func(context.Context) error) { m.nativeClose = close }

func (m *Manager) NativeFilesLocal(ctx context.Context) (bool, error) {
	backend, err := m.backend(ctx)
	return backend.limaInstance == "" && runtime.GOOS == "linux", err
}

func (m *Manager) NativeFilesTarget(ctx context.Context, kind, name string, writable bool) (target nativefiles.Target, err error) {
	descriptor, err := m.FileDescriptor(ctx, kind, name)
	if err != nil {
		return target, err
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return target, err
	}
	if writable && (descriptor.Resource.ReadOnly || kind == "image") {
		return target, fmt.Errorf("%w: native image or read-only container writes are rejected", datafiles.ErrUnsupported)
	}
	if runtime.GOOS == "windows" {
		if backend.limaInstance == "" {
			return target, fmt.Errorf("%w: native Windows runtime files require the Porto Linux guest", datafiles.ErrUnsupported)
		}
		encoded, err := nativefiles.EncodeNativeRequest(descriptor, writable)
		if err != nil {
			return target, err
		}
		target = nativefiles.Target{
			Resource: descriptor.Resource, Identity: descriptor.Resource.Fingerprint(),
			SFTP: &nativefiles.SFTPCommand{
				Name: backend.name,
				Args: []string{"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c",
					`exec sudo -n -- "$HOME/.local/bin/porto-runtime-helper" files-sftp "$1"`, "porto-native-files", encoded},
			},
			Release: func(context.Context) error { return nil },
		}
		target.Verify = func(ctx context.Context) error {
			current, err := m.FileDescriptor(ctx, kind, descriptor.Resource.Name)
			if err != nil {
				return err
			}
			if current.Resource.Fingerprint() != target.Identity || current.PID != descriptor.PID {
				return datafiles.ErrConflict
			}
			return nil
		}
		return target, nil
	}
	if kind == "container" && descriptor.PID > 0 {
		if backend.limaInstance == "" {
			return target, fmt.Errorf("%w: this rootless kernel locks the running container's mount subtree; use in-app Files or stop it explicitly before mounting its snapshot", datafiles.ErrUnsupported)
		}
		config, err := m.discoverLimaSSHConfig(ctx, backend.limaInstance)
		if err != nil {
			return target, err
		}
		target = nativefiles.Target{
			Resource: descriptor.Resource, Identity: descriptor.Resource.Fingerprint(),
			RemotePath: fmt.Sprintf("/proc/%d/root", descriptor.PID),
			SSHConfig:  config, SSHHost: "lima-" + backend.limaInstance,
			Release: func(context.Context) error { return nil },
		}
		target.Verify = func(ctx context.Context) error {
			current, err := m.FileDescriptor(ctx, "container", descriptor.Resource.ID)
			if err != nil {
				return err
			}
			if current.Resource.Fingerprint() != target.Identity || current.PID != descriptor.PID {
				return datafiles.ErrConflict
			}
			return nil
		}
		return target, nil
	}
	var output bytes.Buffer
	err = m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "attach", Identity: descriptor.Resource.Fingerprint(), Writable: writable}, nil, &output)
	if err != nil {
		return target, err
	}
	var attachment runtimefiles.Attachment
	if err := json.Unmarshal(output.Bytes(), &attachment); err != nil {
		return target, err
	}
	target = nativefiles.Target{
		Resource: descriptor.Resource, Identity: descriptor.Resource.Fingerprint(), RemotePath: attachment.Path,
		Backend: &descriptor, Token: attachment.Token, Local: backend.limaInstance == "",
		NamespacePID: attachment.NamespacePID,
	}
	descriptor.NamespacePID = attachment.NamespacePID
	if target.Local && target.NamespacePID > 0 {
		target.RemotePath = fmt.Sprintf("/proc/%d/root%s", target.NamespacePID, attachment.Path)
	}
	target.Release = func(ctx context.Context) error {
		var output bytes.Buffer
		return m.RunFileRequest(ctx, descriptor, datafiles.Request{Action: "detach", Identity: descriptor.Resource.Fingerprint(), Token: attachment.Token}, nil, &output)
	}
	target.Verify = func(ctx context.Context) error {
		current, err := m.FileDescriptor(ctx, kind, descriptor.Resource.Name)
		if err != nil {
			return err
		}
		if current.Resource.Fingerprint() != target.Identity || current.PID != descriptor.PID {
			return datafiles.ErrConflict
		}
		return nil
	}
	if backend.limaInstance != "" {
		config, err := m.discoverLimaSSHConfig(ctx, backend.limaInstance)
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			return target, errors.Join(err, target.Release(cleanup))
		}
		target.SSHConfig, target.SSHHost = config, "lima-"+backend.limaInstance
	}
	return target, nil
}

func (m *Manager) ReleaseNativeRecord(ctx context.Context, record nativefiles.Attachment) error {
	if record.Backend == nil || record.Token == "" {
		return datafiles.ErrInvalid
	}
	var output bytes.Buffer
	return m.RunFileRequest(ctx, *record.Backend, datafiles.Request{
		Action: "detach", Identity: record.Identity, Token: record.Token,
	}, nil, &output)
}
