package localhttps

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

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
