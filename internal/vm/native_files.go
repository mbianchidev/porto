package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/nativefiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) NativeFilesTarget(ctx context.Context, name string, writable bool) (nativefiles.Target, error) {
	if err := m.EnsureStandalone(name); err != nil {
		return nativefiles.Target{}, err
	}
	instance, err := m.Get(ctx, name)
	if err != nil {
		return nativefiles.Target{}, err
	}
	if !strings.EqualFold(instance.Status, "running") {
		return nativefiles.Target{}, fmt.Errorf("%w: managed VM is stopped; start it explicitly or use a VM snapshot, never an implicit workload start", datafiles.ErrUnsupported)
	}
	metadata, err := m.readMetadata(name)
	if err != nil {
		return nativefiles.Target{}, err
	}
	identity := sha256.Sum256([]byte(instance.Directory + "\x00" + metadata.CreatedAt.UTC().Format(time.RFC3339Nano)))
	resource := datafiles.Resource{Kind: "vm", Name: name, ID: hex.EncodeToString(identity[:]), CreatedAt: metadata.CreatedAt.UTC().Format(time.RFC3339Nano), Backend: "Porto-managed Lima"}
	output, err := m.run(ctx, 20*time.Second, "resolve managed VM home for native files", "shell", "--workdir=/", name, "--", "sh", "-c", `printf '%s\n' "$HOME"`)
	if err != nil {
		return nativefiles.Target{}, err
	}
	home := strings.TrimSpace(string(output))
	if !path.IsAbs(home) || strings.ContainsAny(home, "\r\n\x00") {
		return nativefiles.Target{}, errors.New("VM home directory response is invalid")
	}
	config := filepath.Join(instance.Directory, "ssh.config")
	target := nativefiles.Target{Resource: resource, Identity: resource.Fingerprint(), RemotePath: home, SSHConfig: config, SSHHost: "lima-" + name}
	if runtime.GOOS == "windows" {
		executable, err := os.Executable()
		if err != nil {
			return nativefiles.Target{}, err
		}
		install, cancel := context.WithTimeout(ctx, time.Minute)
		digest, err := runtimes.InstallNativeHelper(install, m.runner,
			m.limaCommand([]string{"shell", "--workdir=/", name, "--"}, nil), executable, m.lookPath)
		cancel()
		if err != nil {
			return nativefiles.Target{}, err
		}
		encoded, err := nativefiles.EncodeNativeRequest(runtimefiles.Descriptor{Resource: resource, RootPath: home}, writable)
		if err != nil {
			return nativefiles.Target{}, err
		}
		command := m.limaCommand([]string{
			"shell", "--workdir=/", name, "--", "sh", "-c",
			`exec "$HOME/.local/share/porto/native-helpers/$1/porto-runtime-helper" files-sftp "$2"`,
			"porto-native-files", digest, encoded,
		}, nil)
		target.SFTP = &nativefiles.SFTPCommand{Name: command.Name, Args: command.Args, Env: command.Env}
	}
	target.Release = func(context.Context) error { return nil }
	target.Verify = func(ctx context.Context) error {
		if err := m.EnsureStandalone(name); err != nil {
			return err
		}
		current, err := m.Get(ctx, name)
		if err != nil {
			return err
		}
		currentMetadata, err := m.readMetadata(name)
		if err != nil {
			return err
		}
		if !strings.EqualFold(current.Status, "running") || current.Directory != instance.Directory || !currentMetadata.CreatedAt.Equal(metadata.CreatedAt) {
			return datafiles.ErrConflict
		}
		return nil
	}
	return target, nil
}
