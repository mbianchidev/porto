package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
)

const networkFileOwner = "# Managed by Porto engine networking v1"

func guestNetworkArchive(configuration networking.GuestConfig, owner string) ([]byte, error) {
	var shell, systemd strings.Builder
	shell.WriteString(networkFileOwner + "\n")
	systemd.WriteString(networkFileOwner + "\n")
	for _, entry := range configuration.Environment {
		name, value, found := strings.Cut(entry, "=")
		if !found || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("guest proxy settings must be single-line environment values")
		}
		switch strings.ToUpper(name) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		default:
			return nil, errors.New("guest networking may only configure proxy variables")
		}
		shell.WriteString(name + "=" + shellJoin([]string{value}) + "\n")
		escaped := strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`)
		systemd.WriteString(name + `="` + escaped + "\"\n")
	}
	hasCA := "0"
	if len(configuration.RegistryCAPEM) > 0 {
		hasCA = "1"
	}
	files := map[string][]byte{
		"owner": []byte(owner + "\n"), "generation": []byte(configuration.Generation + "\n"),
		"proxy.env": []byte(shell.String()), "systemd.env": []byte(systemd.String()),
		"registry-additions.pem": configuration.RegistryCAPEM, "has-ca": []byte(hasCA + "\n"),
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for name, data := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			return nil, err
		}
		if _, err := writer.Write(data); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

const applyGuestNetworkScript = `set -eu
umask 077
expected="$1"
[ "$(cat "$HOME/.porto-engine-owner")" = "$expected" ] || { echo 'Engine ownership changed; refusing network configuration' >&2; exit 1; }
base="$HOME/.config/porto"
root="$base/network"
dropin_dir="$HOME/.config/systemd/user/default-buildkit.service.d"
dropin="$dropin_dir/porto-network.conf"
if [ -e "$root" ]; then
  [ ! -L "$root" ] && [ -f "$root/owner" ] && [ "$(cat "$root/owner")" = "$expected" ] || {
    echo 'Existing guest network configuration is not owned by Porto' >&2; exit 1;
  }
fi
[ ! -L "$dropin_dir" ] && [ ! -L "$dropin" ] || { echo 'Refusing a symbolic-link BuildKit override' >&2; exit 1; }
if [ -f "$dropin" ] && ! grep -Fxq '# Managed by Porto engine networking v1' "$dropin"; then
  echo 'Existing BuildKit override is not owned by Porto' >&2; exit 1
fi
mkdir -p "$base" "$dropin_dir"
transaction="$(mktemp -d "$base/.network-stage.XXXXXX")"
mkdir "$transaction/new"
old_root=false
old_dropin=false
changed=false
complete=false
if [ -d "$root" ]; then old_root=true; fi
if [ -f "$dropin" ]; then cp -p -- "$dropin" "$transaction/previous-dropin"; old_dropin=true; fi
cleanup() {
  if [ "$complete" = false ] && [ "$changed" = true ]; then
    rm -rf -- "$root"
    if [ "$old_root" = true ]; then mv -- "$transaction/previous" "$root"; fi
    if [ "$old_dropin" = true ]; then
      cp -p -- "$transaction/previous-dropin" "$dropin"
    else
      rm -f -- "$dropin"
    fi
    systemctl --user daemon-reload
    systemctl --user restart default-buildkit.service
  fi
  rm -rf -- "$transaction"
}
trap cleanup EXIT
tar --no-same-owner -xf - -C "$transaction/new"
[ "$(cat "$transaction/new/owner")" = "$expected" ] || { echo 'Guest network bundle ownership mismatch' >&2; exit 1; }
if [ "$(cat "$transaction/new/has-ca")" = 1 ]; then
  roots_found=false
  for roots in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    if [ -f "$roots" ]; then
      cat "$roots" > "$transaction/new/registry-ca.pem"
      roots_found=true
      break
    fi
  done
  [ "$roots_found" = true ] || { echo 'Guest system CA bundle is unavailable; no trust settings were changed' >&2; exit 1; }
  cat "$transaction/new/registry-additions.pem" >> "$transaction/new/registry-ca.pem"
fi
changed=true
if [ "$old_root" = true ]; then mv -- "$root" "$transaction/previous"; fi
mv -- "$transaction/new" "$root"
{
  printf '%s\n' '# Managed by Porto engine networking v1' '[Service]' 'EnvironmentFile=%h/.config/porto/network/systemd.env'
  if [ "$(cat "$root/has-ca")" = 1 ]; then
    printf '%s\n' 'Environment="SSL_CERT_FILE=%h/.config/porto/network/registry-ca.pem"'
  fi
} > "$transaction/dropin"
chmod 0600 "$transaction/dropin"
mv -- "$transaction/dropin" "$dropin"
systemctl --user daemon-reload
systemctl --user restart default-buildkit.service
systemctl --user is-active --quiet default-buildkit.service
complete=true
`

type networkReceipt struct {
	Generation string `json:"generation"`
	OwnerID    string `json:"ownerId"`
}

func (m *Manager) NetworkConfigurationStatus() (map[string]any, error) {
	manager, err := networking.Default()
	if err != nil {
		return nil, err
	}
	generation, err := manager.Generation()
	if err != nil {
		return nil, err
	}
	var receipt networkReceipt
	data, err := os.ReadFile(filepath.Join(m.stateDir, "network-applied.json"))
	if err == nil {
		if err := json.Unmarshal(data, &receipt); err != nil {
			return nil, fmt.Errorf("read engine networking receipt: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	state, stateErr := m.readEngineState()
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	return map[string]any{
		"configured": generation != "", "needsApply": generation != "" && (generation != receipt.Generation || state.OwnerID != receipt.OwnerID),
		"backend": state.Mode,
	}, nil
}

func (m *Manager) ApplyNetworking(ctx context.Context) error {
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	m.installMu.Lock()
	defer m.installMu.Unlock()
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	if backend.limaInstance == "" {
		return fmt.Errorf("%w: external/local containerd and BuildKit services are administrator-owned; host proxy and registry-client trust are already applied, but configure the external builder environment explicitly", ErrUnsupported)
	}
	return m.applyGuestNetworking(ctx, backend, true)
}

func (m *Manager) applyGuestNetworking(ctx context.Context, backend commandBackend, force bool) error {
	manager, err := networking.Default()
	if err != nil {
		return err
	}
	generation, err := manager.Generation()
	if err != nil {
		return err
	}
	if generation == "" && !force {
		return nil
	}
	if !force {
		status, err := m.NetworkConfigurationStatus()
		if err != nil {
			return err
		}
		if pending, _ := status["needsApply"].(bool); !pending {
			return nil
		}
	}
	configuration, err := manager.GuestConfiguration()
	if err != nil {
		return err
	}
	ctx, release, err := m.BeginDataTransaction(ctx)
	if err != nil {
		return err
	}
	defer release()
	owner := backend.ownerID
	if owner == "" {
		state, err := m.readEngineState()
		if err != nil {
			return err
		}
		owner = state.OwnerID
	}
	if owner == "" || backend.limaInstance != engineInstanceName {
		return errors.New("owned engine identity is required before applying network settings")
	}
	if err := m.verifyLimaOwnership(ctx, owner); err != nil {
		return err
	}
	connection, err := newBuildKitControlConnection(func(ctx context.Context) (net.Conn, error) { return m.dialBuildKitBackend(ctx, backend) })
	if err != nil {
		return err
	}
	idleErr := ensureBuildKitIdle(ctx, controlapi.NewControlClient(connection))
	closeErr := connection.Close()
	if err := errors.Join(idleErr, closeErr); err != nil {
		return fmt.Errorf("finish active builds before applying engine networking: %w", err)
	}
	archive, err := guestNetworkArchive(configuration, owner)
	if err != nil {
		return err
	}
	if _, err := m.runCommand(ctx, time.Minute, "apply owned engine proxy and registry trust", archive,
		"limactl", "shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c", applyGuestNetworkScript, "porto-network", owner); err != nil {
		return err
	}
	current, err := manager.Generation()
	if err != nil {
		return err
	}
	if current != configuration.Generation {
		return fmt.Errorf("%w: networking settings changed while applying them; retry the explicit apply action", ErrConflict)
	}
	receipt, err := json.Marshal(networkReceipt{Generation: configuration.Generation, OwnerID: owner})
	if err != nil {
		return err
	}
	return writeLimaConfiguration(filepath.Join(m.stateDir, "network-applied.json"), receipt, 0o600)
}

func (m *Manager) networkCommandArgs(backend commandBackend, args []string) ([]string, error) {
	if backend.limaInstance == "" {
		if m.IsNativeWindows() && runtimes.RegistryOperation(args) {
			if policy := m.NetworkPolicy(); policy != nil {
				directory, err := policy.RegistryHostsDirectory()
				if err != nil {
					return nil, err
				}
				if directory != "" {
					return append([]string{"--hosts-dir", directory}, args...), nil
				}
			}
		}
		return args, nil
	}
	outbound := false
	for index, argument := range args {
		if argument == "nerdctl" {
			outbound = runtimes.RegistryOperation(args[index+1:])
			break
		}
	}
	if !outbound {
		return args, nil
	}
	manager, err := networking.Default()
	if err != nil {
		return nil, err
	}
	generation, err := manager.Generation()
	if err != nil || generation == "" {
		return args, err
	}
	for index, arg := range args {
		if arg != "--" {
			continue
		}
		script := `set -eu
root="$HOME/.config/porto/network"
[ -f "$root/generation" ] && [ "$(cat "$root/generation")" = '` + generation + `' ] || {
  echo "Porto networking settings need applying; use 'porto networking apply' after finishing active builds" >&2
  exit 1
}
set -a
. "$root/proxy.env"
set +a
if [ "$(cat "$root/has-ca")" = 1 ]; then export SSL_CERT_FILE="$root/registry-ca.pem"; fi
exec "$@"
`
		wrapped := append(append([]string(nil), args[:index+1]...), "sh", "-c", script, "porto-network")
		return append(wrapped, args[index+1:]...), nil
	}
	return nil, errors.New("Lima command does not expose an explicit guest command boundary")
}
