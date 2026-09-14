package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/process"
)

func TestCommandConnHelperProcess(t *testing.T) {
	switch os.Getenv("PORTO_TEST_COMMAND_CONN") {
	case "echo":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "fail":
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "synthetic runtime socket unavailable")
		os.Exit(7)
	case "proxy":
		connection, err := net.Dial("tcp", os.Getenv("PORTO_TEST_TUNNEL_ADDRESS"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		go func() {
			_, _ = io.Copy(connection, os.Stdin)
			_ = connection.Close()
		}()
		_, _ = io.Copy(os.Stdout, connection)
		_ = connection.Close()
		os.Exit(0)
	}
}

func TestCommandConnPropagatesSubprocessFailure(t *testing.T) {
	command := process.NewCommand(context.Background(), "", os.Args[0], "-test.run=^TestCommandConnHelperProcess$")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_TEST_COMMAND_CONN=fail")
	connection, err := dialCommandConn(context.Background(), "containerd tunnel", command, buildKitAddr("host"), buildKitAddr("guest"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(connection); err == nil || !strings.Contains(err.Error(), "synthetic runtime socket unavailable") {
		t.Fatalf("tunnel discarded the subprocess failure: %v", err)
	}
}

func TestCommandConnPreservesFinalOutput(t *testing.T) {
	command := process.NewCommand(context.Background(), "", os.Args[0], "-test.run=^TestCommandConnHelperProcess$")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_TEST_COMMAND_CONN=echo")
	connection, err := dialCommandConn(context.Background(), "containerd tunnel", command, buildKitAddr("host"), buildKitAddr("guest"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	payload := bytes.Repeat([]byte{0, 1, 127, 128, 255}, 128*1024)
	written := make(chan error, 1)
	go func() {
		_, err := connection.Write(payload)
		if err == nil {
			err = connection.(*commandConn).stdin.Close()
		}
		written <- err
	}()
	output, err := io.ReadAll(connection)
	if err != nil {
		t.Fatalf("read tunnel: %v", err)
	}
	if err := <-written; err != nil {
		t.Fatalf("write tunnel: %v", err)
	}
	if !bytes.Equal(output, payload) {
		t.Fatalf("tunnel returned %d bytes, want %d", len(output), len(payload))
	}
}
