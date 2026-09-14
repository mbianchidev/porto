package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRuntimeHelperStdioProcess(t *testing.T) {
	if os.Getenv("PORTO_TEST_STDIO_HELPER") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"porto-runtime-helper"}, os.Args[index+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestRuntimeHelperDialStdioRejectsInvalidAddresses(t *testing.T) {
	for _, args := range [][]string{nil, {"relative.sock"}, {"/tmp/a", "/tmp/b"}, {"/tmp/socket\n"}} {
		err := dialStdio(context.Background(), args, io.NopCloser(strings.NewReader("")), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "absolute Unix socket path") {
			t.Errorf("dial-stdio %q = %v", args, err)
		}
	}
}

func TestRuntimeHelperDialStdioClosesIdleInputAfterPeerExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bundled runtime helper runs inside a Linux guest")
	}
	directory, err := os.MkdirTemp("", "porto-io-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "containerd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		done <- dialStdio(context.Background(), []string{socket}, input, io.Discard)
	}()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relay cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay leaked its input reader after the socket peer closed")
	}
}

func TestRuntimeHelperDialStdioBridgesUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the bundled runtime helper runs inside a Linux guest")
	}
	directory, err := os.MkdirTemp("", "porto-io-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "containerd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request := []byte{0, 1, '\n', '\r', 127, 128, 255}
	response := append([]byte("reply:"), request...)
	serverDone := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		received, err := io.ReadAll(connection)
		if err == nil && !bytes.Equal(received, request) {
			t.Errorf("request = %v, want %v", received, request)
		}
		if err == nil {
			_, err = connection.Write(response)
		}
		serverDone <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeHelperStdioProcess$", "--", "dial-stdio", socket)
	command.Env = append(os.Environ(), "PORTO_TEST_STDIO_HELPER=1")
	command.Stdin = bytes.NewReader(request)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("dial-stdio failed: %v: %s", err, stderr.String())
	}
	if !bytes.Equal(output, response) {
		t.Fatalf("response = %v, want %v", output, response)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("socket server: %v", err)
	}
}
