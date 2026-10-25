package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const limaBuildKitCommand = `
for socket in \
  "$XDG_RUNTIME_DIR/buildkit-default/buildkitd.sock" \
  "$XDG_RUNTIME_DIR/buildkit/buildkitd.sock"
do
  if [ -S "$socket" ]; then
    exec buildctl --addr "unix://$socket" dial-stdio
  fi
done
echo "BuildKit socket is unavailable" >&2
exit 1
`

func (m *Manager) DialBuildKit(ctx context.Context) (net.Conn, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	return m.dialBuildKitBackend(ctx, backend)
}

func (m *Manager) dialBuildKitBackend(ctx context.Context, backend commandBackend) (net.Conn, error) {
	if m.dialBuildKit != nil {
		return m.dialBuildKit(ctx)
	}
	if backend.name != "limactl" {
		return dialLocalBuildKit(ctx)
	}
	if backend.limaInstance == "" {
		return nil, errors.New("Porto Lima backend configuration is incomplete")
	}
	return dialLimaBuildKit(ctx, backend.limaInstance)
}

func dialLocalBuildKit(ctx context.Context) (net.Conn, error) {
	if configured := strings.TrimSpace(os.Getenv("BUILDKIT_HOST")); configured != "" {
		return dialBuildKitAddress(ctx, configured)
	}
	var addresses []string
	if runtimeDirectory := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); runtimeDirectory != "" {
		addresses = append(addresses,
			"unix://"+filepath.Join(runtimeDirectory, "buildkit-default", "buildkitd.sock"),
			"unix://"+filepath.Join(runtimeDirectory, "buildkit", "buildkitd.sock"),
		)
	}
	if current, err := user.Current(); err == nil && current.Uid != "" {
		runtimeDirectory := filepath.Join("/run/user", current.Uid)
		addresses = append(addresses,
			"unix://"+filepath.Join(runtimeDirectory, "buildkit-default", "buildkitd.sock"),
			"unix://"+filepath.Join(runtimeDirectory, "buildkit", "buildkitd.sock"),
		)
	}
	addresses = append(addresses, "unix:///run/buildkit/buildkitd.sock")
	var dialErrors []error
	for _, address := range addresses {
		connection, err := dialBuildKitAddress(ctx, address)
		if err == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, err)
	}
	return nil, fmt.Errorf("%w; BuildKit is not reachable: %w", ErrUnavailable, errors.Join(dialErrors...))
}

func dialBuildKitAddress(ctx context.Context, address string) (net.Conn, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return nil, fmt.Errorf("parse BuildKit address: %w", err)
	}
	switch parsed.Scheme {
	case "unix":
		return (&net.Dialer{}).DialContext(ctx, "unix", parsed.Path)
	case "tcp":
		return (&net.Dialer{}).DialContext(ctx, "tcp", parsed.Host)
	default:
		return nil, fmt.Errorf("%w: BuildKit address scheme %q", ErrUnsupported, parsed.Scheme)
	}
}

func dialLimaBuildKit(ctx context.Context, instance string) (net.Conn, error) {
	command := limaBuildKitStdioCommand(ctx, instance)
	return dialCommandConn(ctx, "BuildKit tunnel", command, buildKitAddr("porto"), buildKitAddr("buildkit"))
}

func limaBuildKitStdioCommand(ctx context.Context, instance string) *exec.Cmd {
	return newTunnelCommand(
		ctx,
		"limactl",
		"shell",
		"--workdir=/",
		instance,
		"--",
		"sh",
		"-lc",
		limaBuildKitCommand,
	)
}

func newTunnelCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	// Dial cancellation must not kill an established connection; Close owns it.
	return process.NewCommand(context.WithoutCancel(ctx), "", name, args...)
}

func dialCommandConn(ctx context.Context, action string, command *exec.Cmd, localAddr, remoteAddr net.Addr) (net.Conn, error) {
	return runtimes.DialCommand(ctx, action, command, localAddr, remoteAddr)
}

type commandConn = runtimes.CommandConn

type buildKitAddr string

func (a buildKitAddr) Network() string {
	return "stdio"
}

func (a buildKitAddr) String() string {
	return string(a)
}
