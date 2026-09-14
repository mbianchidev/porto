package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"
)

func dialStdio(ctx context.Context, args []string, input io.ReadCloser, output io.Writer) error {
	if len(args) != 1 || !path.IsAbs(args[0]) || strings.ContainsAny(args[0], "\x00\r\n") {
		return errors.New("usage: porto-runtime-helper dial-stdio <absolute Unix socket path>")
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", args[0])
	if err != nil {
		return fmt.Errorf("connect runtime socket %q: %w", args[0], err)
	}
	socket, ok := connection.(*net.UnixConn)
	if !ok {
		return errors.Join(errors.New("runtime socket is not a Unix connection"), connection.Close())
	}
	inputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(socket, input)
		if copyErr == nil {
			copyErr = socket.CloseWrite()
		}
		inputDone <- copyErr
		if copyErr != nil {
			_ = socket.Close()
		}
	}()

	_, outputErr := io.Copy(output, socket)
	inputCloseErr := input.Close()
	socketCloseErr := socket.Close()
	inputErr := <-inputDone
	return errors.Join(
		stdioError("forward runtime input", inputErr),
		stdioError("forward runtime output", outputErr),
		stdioError("close runtime input", inputCloseErr),
		stdioError("close runtime socket", socketCloseErr),
	)
}

func stdioError(action string, err error) error {
	if err == nil || errors.Is(err, os.ErrClosed) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}
