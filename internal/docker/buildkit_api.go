package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func (a *API) buildKitControl(w http.ResponseWriter, r *http.Request) {
	tunnelContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	backend, err := a.manager.DialBuildKit(tunnelContext)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	a.hijackBuildKit(tunnelContext, w, r, backend, nil)
}

func (a *API) buildKitSession(w http.ResponseWriter, r *http.Request) {
	tunnelContext, cancel := context.WithCancel(dockerServerContext(r.Context()))
	defer cancel()
	session, err := dialBuildKitSession(tunnelContext, a.manager.DialBuildKit, r.Header)
	if err != nil {
		writeDockerError(w, err)
		return
	}
	a.hijackBuildKit(tunnelContext, w, r, session, nil)
}

func dialBuildKitSession(
	ctx context.Context,
	dial func(context.Context) (net.Conn, error),
	headers http.Header,
) (net.Conn, error) {
	connection, err := grpc.NewClient(
		"passthrough:///buildkit",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return dial(ctx)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to BuildKit session service: %w", err)
	}
	sessionContext := metadata.NewOutgoingContext(ctx, lowerBuildKitHeaders(headers))
	session, err := controlapi.NewControlClient(connection).Session(sessionContext)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("open BuildKit session: %w", err)
	}
	return &buildKitSessionConn{stream: session, client: connection}, nil
}

func (a *API) hijackBuildKit(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	backend net.Conn,
	closeBackendClient func() error,
) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "h2c") {
		_ = backend.Close()
		if closeBackendClient != nil {
			_ = closeBackendClient()
		}
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "BuildKit requires an h2c connection upgrade"})
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = backend.Close()
		if closeBackendClient != nil {
			_ = closeBackendClient()
		}
		writeDockerJSON(w, http.StatusInternalServerError, map[string]string{"message": "Docker API connection cannot be hijacked"})
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = backend.Close()
		if closeBackendClient != nil {
			_ = closeBackendClient()
		}
		return
	}
	if err := writeBuildKitUpgrade(buffered); err != nil {
		_ = client.Close()
		_ = backend.Close()
		if closeBackendClient != nil {
			_ = closeBackendClient()
		}
		return
	}
	bridgeConnections(ctx, client, backend)
	if closeBackendClient != nil {
		_ = closeBackendClient()
	}
}

func writeBuildKitUpgrade(connection *bufio.ReadWriter) error {
	if _, err := connection.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"); err != nil {
		return err
	}
	return connection.Flush()
}

func bridgeConnections(ctx context.Context, client, backend net.Conn) {
	errorsChannel := make(chan error, 2)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = client.Close()
			_ = backend.Close()
		case <-stopped:
		}
	}()
	go func() {
		_, err := io.Copy(backend, client)
		errorsChannel <- err
	}()
	go func() {
		_, err := io.Copy(client, backend)
		errorsChannel <- err
	}()
	<-errorsChannel
	_ = client.Close()
	_ = backend.Close()
	<-errorsChannel
	close(stopped)
}

type buildKitSessionConn struct {
	stream    grpc.BidiStreamingClient[controlapi.BytesMessage, controlapi.BytesMessage]
	client    *grpc.ClientConn
	lastRead  []byte
	readMutex sync.Mutex
	writeMu   sync.Mutex
	once      sync.Once
}

func (c *buildKitSessionConn) Read(data []byte) (int, error) {
	c.readMutex.Lock()
	defer c.readMutex.Unlock()
	if len(c.lastRead) > 0 {
		count := copy(data, c.lastRead)
		c.lastRead = c.lastRead[count:]
		return count, nil
	}
	message, err := c.stream.Recv()
	if err != nil {
		return 0, err
	}
	count := copy(data, message.Data)
	c.lastRead = append(c.lastRead[:0], message.Data[count:]...)
	return count, nil
}

func (c *buildKitSessionConn) Write(data []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	message := &controlapi.BytesMessage{Data: append([]byte(nil), data...)}
	if err := c.stream.Send(message); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (c *buildKitSessionConn) Close() error {
	var closeErr error
	c.once.Do(func() {
		closeErr = c.client.Close()
		c.writeMu.Lock()
		closeErr = errors.Join(closeErr, c.stream.CloseSend())
		c.writeMu.Unlock()
	})
	return closeErr
}

func (c *buildKitSessionConn) LocalAddr() net.Addr {
	return buildKitAddr("porto-session")
}

func (c *buildKitSessionConn) RemoteAddr() net.Addr {
	return buildKitAddr("buildkit-session")
}

func (c *buildKitSessionConn) SetDeadline(time.Time) error {
	return nil
}

func (c *buildKitSessionConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *buildKitSessionConn) SetWriteDeadline(time.Time) error {
	return nil
}

func lowerBuildKitHeaders(headers http.Header) metadata.MD {
	values := make(metadata.MD)
	for key, entries := range headers {
		lowerKey := strings.ToLower(key)
		if strings.HasPrefix(lowerKey, "x-docker-expose-session-") {
			values[lowerKey] = append([]string(nil), entries...)
		}
	}
	return values
}
