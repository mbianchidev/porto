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
	"syscall"
	"time"

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
	command := process.NewCommand(
		ctx,
		"",
		"limactl",
		"shell",
		"--workdir=/",
		instance,
		"--",
		"sh",
		"-lc",
		limaBuildKitCommand,
	)
	return dialCommandConn(ctx, "BuildKit tunnel", command, buildKitAddr("porto"), buildKitAddr("buildkit"))
}

func dialCommandConn(ctx context.Context, action string, command *exec.Cmd, localAddr, remoteAddr net.Addr) (net.Conn, error) {
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open %s input: %w", action, err)
	}
	stdout, output := io.Pipe()
	command.Stdout = output
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, fmt.Errorf("start %s: %w", action, err)
	}
	connection := &commandConn{
		command:    command,
		stdin:      stdin,
		stdout:     stdout,
		action:     action,
		done:       make(chan struct{}),
		localAddr:  localAddr,
		remoteAddr: remoteAddr,
	}
	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			waitErr = runtimes.CommandError(action, stderr.Bytes(), waitErr)
		}
		connection.waitErr = waitErr
		_ = output.CloseWithError(waitErr)
		close(connection.done)
	}()
	select {
	case <-connection.done:
		_ = stdin.Close()
		_ = stdout.Close()
		if connection.waitErr != nil {
			return nil, fmt.Errorf("%s exited before connecting: %w", action, connection.waitErr)
		}
		return nil, fmt.Errorf("%s exited before connecting", action)
	case <-time.After(100 * time.Millisecond):
		return connection, nil
	case <-ctx.Done():
		_ = connection.Close()
		return nil, context.Cause(ctx)
	}
}

type commandConn struct {
	command    *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	action     string
	done       chan struct{}
	waitErr    error
	once       sync.Once
	closeErr   error
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (c *commandConn) Read(data []byte) (int, error) {
	return c.stdout.Read(data)
}

func (c *commandConn) Write(data []byte) (int, error) {
	return c.stdin.Write(data)
}

func (c *commandConn) Close() error {
	c.once.Do(func() {
		for _, err := range []error{c.stdin.Close(), c.stdout.Close()} {
			if err != nil && !errors.Is(err, os.ErrClosed) {
				c.closeErr = errors.Join(c.closeErr, err)
			}
		}
		select {
		case <-c.done:
			return
		default:
		}
		if c.command.Process != nil {
			if err := process.Kill(c.command); err != nil &&
				!errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
				c.closeErr = errors.Join(c.closeErr, err)
			}
		}
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			c.closeErr = errors.Join(c.closeErr, fmt.Errorf("timed out stopping %s", c.action))
		}
	})
	return c.closeErr
}

func (c *commandConn) LocalAddr() net.Addr {
	return c.localAddr
}

func (c *commandConn) RemoteAddr() net.Addr {
	return c.remoteAddr
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
