package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"

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

func (m *Manager) nativeFilesDescriptor(ctx context.Context, kind, name string, writable bool) (descriptor runtimefiles.Descriptor, backend commandBackend, err error) {
	descriptor, err = m.FileDescriptor(ctx, kind, name)
	if err != nil {
		return descriptor, backend, err
	}
	if writable && (descriptor.Resource.ReadOnly || kind == "image") {
		return descriptor, backend, fmt.Errorf("%w: native image or read-only container writes are rejected", datafiles.ErrUnsupported)
	}
	backend, err = m.backend(ctx)
	if err != nil {
		return descriptor, backend, err
	}
	if backend.limaInstance == "" {
		if runtime.GOOS != "linux" {
			return descriptor, backend, fmt.Errorf("%w: native local runtime files require Linux; use the Porto Lima engine", datafiles.ErrUnsupported)
		}
	}
	return descriptor, backend, nil
}

func (m *Manager) NativeFilesResource(ctx context.Context, kind, name string, writable bool) (datafiles.Resource, bool, error) {
	descriptor, backend, err := m.nativeFilesDescriptor(ctx, kind, name, writable)
	return descriptor.Resource, backend.limaInstance == "" && runtime.GOOS == "linux" && !(kind == "container" && descriptor.PID > 0), err
}

func (m *Manager) NativeFilesTarget(ctx context.Context, kind, name string, writable bool) (target nativefiles.Target, err error) {
	descriptor, backend, err := m.nativeFilesDescriptor(ctx, kind, name, writable)
	if err != nil {
		return target, err
	}
	if backend.limaInstance != "" || kind == "container" && descriptor.PID > 0 {
		encoded, err := nativefiles.EncodeNativeRequest(descriptor, writable)
		if err != nil {
			return target, err
		}
		target = nativefiles.Target{
			Resource: descriptor.Resource, Identity: descriptor.Resource.Fingerprint(),
			Release: func(context.Context) error { return nil },
		}
		if backend.limaInstance != "" {
			target.SFTP = &nativefiles.SFTPCommand{Name: backend.name, Args: []string{
				"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c",
				`exec sudo -n -- "$HOME/.local/bin/porto-runtime-helper" files-sftp "$1"`, "porto-native-files", encoded,
			}}
		} else {
			helper, err := m.runtimeHelperPath()
			if err != nil || helper == "" {
				return target, errors.Join(errors.New("the native runtime helper is unavailable"), err)
			}
			target.SFTP = &nativefiles.SFTPCommand{Name: helper, Args: []string{"files-sftp", encoded}}
			if os.Geteuid() != 0 {
				target.SFTP.Name = "sudo"
				target.SFTP.Args = []string{"-n", "--", helper, "files-sftp", encoded}
			}
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
