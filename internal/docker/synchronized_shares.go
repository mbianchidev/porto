package docker

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/syncshares"
)

type ownedSyncShareProvider struct{ manager *Manager }

func (m *Manager) SynchronizedShareProvider() syncshares.Provider {
	return ownedSyncShareProvider{manager: m}
}

func (provider ownedSyncShareProvider) Endpoint(ctx context.Context, id string, create bool) (syncshares.Endpoint, error) {
	m := provider.manager
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 12 {
		return syncshares.Endpoint{}, errors.New("invalid owned share identifier")
	}
	state, err := m.readEngineState()
	if err != nil {
		return syncshares.Endpoint{}, err
	}
	if state.Mode != "lima" || state.Instance != engineInstanceName || state.OwnerID == "" {
		return syncshares.Endpoint{}, fmt.Errorf("%w: optimized sharing requires the Porto-owned Lima engine", ErrUnsupported)
	}
	if _, err := m.backend(ctx); err != nil {
		return syncshares.Endpoint{}, err
	}
	instance, err := m.engineConfiguration(ctx, state.Instance)
	if err != nil {
		return syncshares.Endpoint{}, err
	}
	if instance.SSHLocalPort < 1 || instance.SSHLocalPort > 65535 {
		return syncshares.Endpoint{}, errors.New("the owned engine's loopback SSH endpoint is unavailable")
	}
	sshPath := filepath.Join(instance.Directory, "ssh.config")
	sshInfo, err := os.Lstat(sshPath)
	if err != nil {
		return syncshares.Endpoint{}, fmt.Errorf("inspect owned engine SSH configuration: %w", err)
	}
	if !sshInfo.Mode().IsRegular() || sshInfo.Size() > 64<<10 {
		return syncshares.Endpoint{}, errors.New("owned engine SSH configuration must be a bounded regular file")
	}
	sshConfiguration, err := os.ReadFile(sshPath)
	if err != nil {
		return syncshares.Endpoint{}, err
	}
	publicKey, err := m.synchronizedShareOutput(ctx, "verify owned guest SSH host key",
		"shell", "--workdir=/", state.Instance, "--", "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	if err != nil {
		return syncshares.Endpoint{}, err
	}
	fields := strings.Fields(string(publicKey))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" || strings.ContainsAny(fields[1], "\r\n\x00") {
		return syncshares.Endpoint{}, errors.New("the owned guest did not return a valid public SSH host key")
	}
	guestPath := "/mnt/porto-sync/" + id
	if create {
		const script = `set -eu
id="$1"; owner="$2"
root=/mnt/porto-sync
directory="$root/$id"
marker="$root/.owners/$id"
user="$(id -un)"; group="$(id -gn)"
if [ -e "$root" ] || [ -L "$root" ]; then
  [ -d "$root" ] && [ ! -L "$root" ] && [ -f "$root/.engine-owner" ] && [ "$(cat "$root/.engine-owner")" = "$owner" ] || {
    echo "Existing synchronization root is not owned by this engine; no unrelated directory was changed" >&2
    exit 1
  }
else
  sudo -n install -d -m 0700 -o "$user" -g "$group" "$root" "$root/.owners"
  (umask 077; printf '%s\n' "$owner" > "$root/.engine-owner")
fi
if [ -e "$directory" ] || [ -L "$directory" ]; then
  [ -d "$directory" ] && [ ! -L "$directory" ] && [ -f "$marker" ] && [ "$(cat "$marker")" = "$owner" ] || {
    echo "Existing guest share directory is not owned by this request; no data was replaced" >&2
    exit 1
  }
else
  mkdir -m 0700 "$directory"
  (umask 077; printf '%s\n' "$owner" > "$marker")
fi
`
		if _, err := m.runCommand(ctx, 30*time.Second, "prepare only the selected owned guest share", nil,
			"limactl", "shell", "--workdir=/", state.Instance, "--", "sh", "-c", script, "porto-sync-root", id, state.OwnerID); err != nil {
			return syncshares.Endpoint{}, err
		}
	}
	alias := "lima-" + state.Instance
	identity, err := m.synchronizedShareOutput(ctx, "verify guest share root identity",
		"shell", "--workdir=/", state.Instance, "--", "sh", "-c",
		`if [ -e "$1" ] || [ -L "$1" ]; then
  test -d "$1" && test ! -L "$1"
  root=/mnt/porto-sync
  [ -f "$root/.engine-owner" ] && [ "$(cat "$root/.engine-owner")" = "$2" ] &&
    [ -f "$root/.owners/$3" ] && [ "$(cat "$root/.owners/$3")" = "$2" ] || {
    echo "Guest share ownership changed; no replacement root was adopted" >&2
    exit 1
  }
  birth="$(stat -c %w "$1")"
  [ "$birth" != - ] || { echo "Guest root birth time is unavailable" >&2; exit 1; }
  stat -c '%d:%i:%w' "$1"
fi`, "porto-sync-identity", guestPath, state.OwnerID, id)
	if err != nil {
		return syncshares.Endpoint{}, err
	}
	key := fields[0] + " " + fields[1] + "\n"
	knownHosts := fmt.Sprintf("[127.0.0.1]:%d %s[::1]:%d %s", instance.SSHLocalPort, key, instance.SSHLocalPort, key)
	return syncshares.Endpoint{
		OwnerID: state.OwnerID, URL: alias + ":" + guestPath, GuestPath: guestPath, Host: alias,
		SSHConfig: sshConfiguration, KnownHosts: []byte(knownHosts),
		GuestIdentity: strings.TrimSpace(string(identity)),
	}, nil
}

func (m *Manager) synchronizedShareOutput(ctx context.Context, action string, arguments ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	err := m.runStreamingCommand(ctx, 20*time.Second, action, runtimes.Command{
		Name: "limactl", Args: arguments,
	}, func(chunk runtimes.OutputChunk) error {
		output := &stdout
		if chunk.Stream == "stderr" {
			output = &stderr
		} else if chunk.Stream != "stdout" {
			return fmt.Errorf("unexpected owned share output stream %q", chunk.Stream)
		}
		if output.Len()+len(chunk.Data) > 64<<10 {
			return errors.New("owned share command exceeded its output limit")
		}
		_, err := output.Write(chunk.Data)
		return err
	})
	if err != nil {
		return nil, err
	}
	if stderr.Len() > 0 {
		log.Printf("%s: %s", action, networking.Redact(strings.TrimSpace(stderr.String())))
	}
	return stdout.Bytes(), nil
}

func (provider ownedSyncShareProvider) InUse(_ context.Context, root string) error {
	snapshot := provider.manager.ContainerSnapshot()
	if !snapshot.Available || snapshot.Stale {
		return errors.New("a fresh container inventory is required before detaching a synchronized share")
	}
	for _, container := range snapshot.Containers {
		if !container.TaskPresent {
			continue
		}
		for _, mount := range container.MountDetails {
			if mount.Source == root || strings.HasPrefix(mount.Source, root+"/") {
				return fmt.Errorf("%w: container %s still mounts the selected guest share", ErrConflict, container.Name)
			}
		}
	}
	return nil
}

func (m *Manager) SetSynchronizedShareResolver(resolve func(context.Context, string) (string, error)) {
	m.syncResolve = resolve
}

func (m *Manager) prepareSynchronizedMounts(ctx context.Context, request *CreateContainerRequest) error {
	if m.syncResolve == nil || m.IsNativeWindows() || request.Labels["dev.porto.share-mode"] == "bind" {
		return nil
	}
	request.Volumes = append([]string(nil), request.Volumes...)
	for index, volume := range request.Volumes {
		parts := strings.SplitN(volume, ":", 3)
		if len(parts) < 2 || !filepath.IsAbs(parts[0]) {
			continue
		}
		source, err := m.syncResolve(ctx, parts[0])
		if err != nil {
			return err
		}
		parts[0] = source
		request.Volumes[index] = strings.Join(parts, ":")
	}
	request.Mounts = append([]string(nil), request.Mounts...)
	for index, mount := range request.Mounts {
		fields, err := csv.NewReader(strings.NewReader(mount)).Read()
		if err != nil {
			return fmt.Errorf("decode synchronized bind options: %w", err)
		}
		bind := false
		for _, field := range fields {
			bind = bind || field == "type=bind"
		}
		if !bind {
			continue
		}
		for position, field := range fields {
			key, source, _ := strings.Cut(field, "=")
			if key == "source" || key == "src" {
				resolved, err := m.syncResolve(ctx, source)
				if err != nil {
					return err
				}
				fields[position] = key + "=" + resolved
			}
		}
		request.Mounts[index], err = dockerCSV(fields)
		if err != nil {
			return err
		}
	}
	return nil
}
