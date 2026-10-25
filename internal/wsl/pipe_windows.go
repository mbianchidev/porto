//go:build windows

package wsl

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/Microsoft/go-winio"
)

func PipeStdio(ctx context.Context, endpoint string, input io.Reader, output io.Writer) error {
	connection, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	writes := make(chan error, 1)
	go func() {
		_, err := io.Copy(connection, input)
		if closer, ok := connection.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, closer.CloseWrite())
		}
		writes <- err
	}()
	_, err = io.Copy(output, connection)
	_ = connection.Close()
	select {
	case writeErr := <-writes:
		if writeErr != nil && !errors.Is(writeErr, net.ErrClosed) {
			err = errors.Join(err, writeErr)
		}
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
