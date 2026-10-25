package localhttps

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestForwarderAddressesMustBeLiteralLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:443", "[::]:443", "192.0.2.1:443", "localhost:443", ":443"} {
		if err := validateLoopbackAddress(address); err == nil {
			t.Errorf("accepted non-loopback address %q", address)
		}
	}
	for _, address := range []string{ListenAddress, ListenAddressIPv6, TargetAddress, "127.0.0.1:0"} {
		if err := validateLoopbackAddress(address); err != nil {
			t.Errorf("rejected loopback address %q: %v", address, err)
		}
	}
}

func TestForwarderCancellationClosesActiveConnections(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := backend.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	_ = probe.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunForwarder(ctx, address, backend.Addr().String()) }()
	var client net.Conn
	for range 100 {
		client, err = net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(time.Second):
		t.Fatal("forwarder did not connect to backend")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not stop")
	}
	_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("active connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("cancellation leaked an active forwarded connection")
	}
}

func TestRunForwarderProxiesTCP(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		connection, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forwardAddress := probe.Addr().String()
	_ = probe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunForwarder(ctx, forwardAddress, backend.Addr().String())
	}()

	var connection net.Conn
	for range 20 {
		connection, err = net.DialTimeout("tcp", forwardAddress, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("connect to forwarder: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("secure")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("secure"))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "secure" {
		t.Fatalf("response = %q", response)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwarder stopped with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarder did not stop")
	}
}

func TestRunForwardersProxyIPv4AndIPv6(t *testing.T) {
	probe, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	_, rawPort, err := net.SplitHostPort(probe.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = probe.Close()

	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for range 2 {
			connection, acceptErr := backend.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()

	addresses := []string{"127.0.0.1:" + rawPort, "[::1]:" + rawPort}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunForwarders(ctx, addresses, backend.Addr().String())
	}()

	for _, address := range addresses {
		var connection net.Conn
		for range 20 {
			connection, err = net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("connect to %s: %v", address, err)
		}
		if _, err := connection.Write([]byte(address)); err != nil {
			_ = connection.Close()
			t.Fatal(err)
		}
		response := make([]byte, len(address))
		if _, err := io.ReadFull(connection, response); err != nil {
			_ = connection.Close()
			t.Fatal(err)
		}
		_ = connection.Close()
		if string(response) != address {
			t.Fatalf("response = %q, want %q", response, address)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forwarders stopped with error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forwarders did not stop")
	}
}
