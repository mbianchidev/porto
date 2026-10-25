//go:build windows

package docker

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"github.com/Microsoft/go-winio"
	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func allocateWindowsPipes(i windowscontainers.Installation, id string, tty, stdin bool, hub *windowsIOHub) (err error) {
	prefix := `\\.\pipe\porto-windows-io-` + windowscontainers.OwnerKey(i.OwnerSID) + "-" + id
	var mu sync.Mutex
	var closers []io.Closer
	closed := false
	var closeOnce sync.Once
	hub.closePipes = func() {
		closeOnce.Do(func() {
			mu.Lock()
			closed = true
			pending := append([]io.Closer(nil), closers...)
			mu.Unlock()
			for _, closer := range pending {
				if err := closer.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, os.ErrClosed) {
					hub.reportTransportFailure("close named pipe", err, false)
				}
			}
		})
	}
	defer func() {
		if err != nil {
			hub.closePipes()
		}
	}()
	var outputs sync.WaitGroup
	create := func(stream string, input bool) error {
		path := prefix + "-" + stream
		listener, err := winio.ListenPipe(path, &winio.PipeConfig{
			SecurityDescriptor: "D:P(A;;GA;;;SY)(A;;GA;;;BA)", InputBufferSize: 64 * 1024, OutputBufferSize: 64 * 1024})
		if err != nil {
			return fmt.Errorf("create protected native runhcs I/O pipe: %w", err)
		}
		mu.Lock()
		closers = append(closers, listener)
		mu.Unlock()
		if input {
			hub.config.Stdin = path
		} else if stream == "stdout" {
			hub.config.Stdout = path
		} else {
			hub.config.Stderr = path
		}
		if !input {
			outputs.Add(1)
		}
		go func() {
			if !input {
				defer outputs.Done()
			}
			hub.serveWindowsPipe(listener, stream, input, func() bool { mu.Lock(); defer mu.Unlock(); return closed }, func(connection net.Conn) bool {
				mu.Lock()
				defer mu.Unlock()
				if closed {
					return false
				}
				closers = append(closers, connection)
				return true
			})
		}()
		return nil
	}
	if stdin {
		if err := create("stdin", true); err != nil {
			return err
		}
	}
	if err := create("stdout", false); err != nil {
		return err
	}
	if !tty {
		if err := create("stderr", false); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	hub.outputsDone = done
	go func() { outputs.Wait(); close(done) }()
	return nil
}
