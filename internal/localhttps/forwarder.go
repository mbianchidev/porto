package localhttps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid HTTPS forwarder address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("HTTPS forwarder requires a literal loopback address, not %q", address)
	}
	return nil
}

const (
	ListenAddress     = "127.0.0.1:443"
	ListenAddressIPv6 = "[::1]:443"
	TargetAddress     = "127.0.0.1:37681"
)

func RunForwarders(ctx context.Context, listenAddresses []string, targetAddress string) error {
	if len(listenAddresses) == 0 {
		return errors.New("at least one HTTPS listen address is required")
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(listenAddresses))
	for _, listenAddress := range listenAddresses {
		go func() {
			results <- RunForwarder(runContext, listenAddress, targetAddress)
		}()
	}

	var result error
	for range listenAddresses {
		if err := <-results; err != nil {
			result = errors.Join(result, err)
			cancel()
		}
	}
	return result
}

func RunForwarder(ctx context.Context, listenAddress, targetAddress string) error {
	for _, address := range []string{listenAddress, targetAddress} {
		if err := validateLoopbackAddress(address); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddress, err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	var connections sync.WaitGroup
	defer func() {
		cancel()
		connections.Wait()
	}()
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept HTTPS connection: %w", err)
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			forwardConnection(ctx, connection, targetAddress)
		}()
	}
}

func forwardConnection(ctx context.Context, client net.Conn, targetAddress string) {
	defer client.Close()
	stopClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClient()
	target, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", targetAddress)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("HTTPS router is unavailable; start the Porto daemon", "target", targetAddress, "error", err)
		}
		return
	}
	defer target.Close()
	stopTarget := context.AfterFunc(ctx, func() { _ = target.Close() })
	defer stopTarget()

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
