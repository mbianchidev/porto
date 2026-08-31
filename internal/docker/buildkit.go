package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
	if len(backend.prefix) < 2 {
		return nil, errors.New("Porto Lima backend configuration is incomplete")
	}
	return dialLimaBuildKit(ctx, backend.prefix[1])
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
	command := exec.CommandContext(
		ctx,
		"limactl",
		"shell",
		instance,
		"--",
		"sh",
		"-lc",
		limaBuildKitCommand,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open BuildKit tunnel input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("open BuildKit tunnel output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start BuildKit tunnel: %w", err)
	}
	connection := &commandConn{
		command: command,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  &stderr,
		done:    make(chan error, 1),
	}
	go func() {
		connection.done <- command.Wait()
	}()
	select {
	case err := <-connection.done:
		_ = stdin.Close()
		_ = stdout.Close()
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("BuildKit tunnel exited before connecting: %s", message)
	case <-time.After(100 * time.Millisecond):
		return connection, nil
	case <-ctx.Done():
		_ = connection.Close()
		return nil, context.Cause(ctx)
	}
}

type commandConn struct {
	command *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  *bytes.Buffer
	done    chan error
	once    sync.Once
}

func (c *commandConn) Read(data []byte) (int, error) {
	return c.stdout.Read(data)
}

func (c *commandConn) Write(data []byte) (int, error) {
	return c.stdin.Write(data)
}

func (c *commandConn) Close() error {
	var closeErr error
	c.once.Do(func() {
		closeErr = errors.Join(c.stdin.Close(), c.stdout.Close())
		if c.command.Process != nil {
			closeErr = errors.Join(closeErr, c.command.Process.Kill())
		}
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			closeErr = errors.Join(closeErr, errors.New("timed out stopping BuildKit tunnel"))
		}
	})
	return closeErr
}

func (c *commandConn) LocalAddr() net.Addr {
	return buildKitAddr("porto")
}

func (c *commandConn) RemoteAddr() net.Addr {
	return buildKitAddr("buildkit")
}

func (c *commandConn) SetDeadline(time.Time) error {
	return nil
}

func (c *commandConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *commandConn) SetWriteDeadline(time.Time) error {
	return nil
}

type buildKitAddr string

func (a buildKitAddr) Network() string {
	return "stdio"
}

func (a buildKitAddr) String() string {
	return string(a)
}
