package docker

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestLimaContainerdStdioCommandAvoidsHostSocketForwarding(t *testing.T) {
	socket := "/run/user/1000/containerd/containerd.sock"
	command := limaContainerdStdioCommand(context.Background(), "test-engine", socket)
	expected := []string{
		"limactl", "shell", "--workdir=/", "test-engine", "--", "sh", "-c",
		`exec "$HOME/.local/bin/porto-runtime-helper" dial-stdio "$1"`,
		"porto-containerd", socket,
	}
	if !reflect.DeepEqual(command.Args, expected) {
		t.Fatalf("stdio tunnel arguments = %q, want %q", command.Args, expected)
	}
}

func TestLimaContainerdStdioSurvivesDialCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	command := limaContainerdStdioCommand(ctx, "test-engine", "/run/containerd/containerd.sock")
	command.Path = os.Args[0]
	command.Args = []string{os.Args[0], "-test.run=^TestCommandConnHelperProcess$"}
	command.Err = nil
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_TEST_COMMAND_CONN=echo")
	connection, err := dialCommandConn(ctx, "containerd tunnel", command, buildKitAddr("host"), buildKitAddr("guest"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	cancel()

	received := make(chan error, 1)
	go func() {
		if _, err := connection.Write([]byte("ping")); err != nil {
			received <- err
			return
		}
		data := make([]byte, 4)
		_, err := io.ReadFull(connection, data)
		if err == nil && string(data) != "ping" {
			t.Errorf("unexpected tunnel response: %q", data)
		}
		received <- err
	}()
	select {
	case err := <-received:
		if err != nil {
			t.Fatalf("dial cancellation closed the established connection: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("established containerd tunnel stopped responding")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close containerd tunnel: %v", err)
	}
	select {
	case <-connection.(*commandConn).done:
	default:
		t.Fatal("closing the tunnel did not reap its subprocess")
	}
}
