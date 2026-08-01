package localhttps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

const (
	ListenAddress = "127.0.0.1:443"
	TargetAddress = "127.0.0.1:37681"
)

func RunForwarder(ctx context.Context, listenAddress, targetAddress string) error {
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddress, err)
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept HTTPS connection: %w", err)
		}
		go forwardConnection(connection, targetAddress)
	}
}

func forwardConnection(client net.Conn, targetAddress string) {
	defer client.Close()
	target, err := net.Dial("tcp", targetAddress)
	if err != nil {
		return
	}
	defer target.Close()

	var wait sync.WaitGroup
	wait.Add(2)
	go proxyHalf(&wait, target, client)
	go proxyHalf(&wait, client, target)
	wait.Wait()
}

func proxyHalf(wait *sync.WaitGroup, destination, source net.Conn) {
	defer wait.Done()
	_, _ = io.Copy(destination, source)
	if tcp, ok := destination.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
}
