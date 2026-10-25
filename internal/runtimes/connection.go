package runtimes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func DialCommand(ctx context.Context, action string, command *exec.Cmd, localAddr, remoteAddr net.Addr) (net.Conn, error) {
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
	connection := &CommandConn{
		command: command, stdin: stdin, stdout: stdout, action: action,
		done: make(chan struct{}), localAddr: localAddr, remoteAddr: remoteAddr,
	}
	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			waitErr = CommandError(action, stderr.Bytes(), waitErr)
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

type CommandConn struct {
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

func (c *CommandConn) Read(data []byte) (int, error)  { return c.stdout.Read(data) }
func (c *CommandConn) Write(data []byte) (int, error) { return c.stdin.Write(data) }
func (c *CommandConn) CloseWrite() error              { return c.stdin.Close() }

func (c *CommandConn) Close() error {
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
		var killErr error
		if c.command.Process != nil {
			killErr = process.Kill(c.command)
		}
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			c.closeErr = errors.Join(c.closeErr, killErr, fmt.Errorf("timed out stopping %s", c.action))
		}
	})
	return c.closeErr
}

func (c *CommandConn) LocalAddr() net.Addr              { return c.localAddr }
func (c *CommandConn) RemoteAddr() net.Addr             { return c.remoteAddr }
func (c *CommandConn) SetDeadline(time.Time) error      { return nil }
func (c *CommandConn) SetReadDeadline(time.Time) error  { return nil }
func (c *CommandConn) SetWriteDeadline(time.Time) error { return nil }

func (c *CommandConn) Done() <-chan struct{}          { return c.done }
func (c *CommandConn) ProcessState() *os.ProcessState { return c.command.ProcessState }
