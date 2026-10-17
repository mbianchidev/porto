package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestLimaContainerdUsesHelperWithoutIndependentSSHForward(t *testing.T) {
	fakeLimaContainerdStdio(t, "echo")
	ctx, cancel := context.WithCancel(context.Background())
	connection, err := dialLimaContainerd(ctx, "synthetic-engine", "/run/synthetic/containerd.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	cancel()
	assertLimaContainerdEcho(t, connection)
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	channel := connection.(*commandConn)
	select {
	case <-channel.done:
	default:
		t.Fatal("closing the helper channel did not reap its own subprocess")
	}
	if channel.command.ProcessState == nil {
		t.Fatal("closing the channel left its subprocess unreaped")
	}
}

func TestLimaContainerdHelperFailureIsExplicit(t *testing.T) {
	for _, mode := range []string{"fail", "exit"} {
		t.Run(mode, func(t *testing.T) {
			fakeLimaContainerdStdio(t, mode)
			connection, err := dialLimaContainerd(context.Background(), "synthetic-engine", "/run/synthetic/containerd.sock")
			if err == nil {
				defer connection.Close()
				_, err = connection.Read(make([]byte, 1))
			}
			if mode == "fail" && (err == nil || !strings.Contains(err.Error(), "synthetic runtime socket unavailable")) {
				t.Fatalf("helper failure lost its diagnostic: %v", err)
			}
			if mode == "exit" && !errors.Is(err, io.EOF) && (err == nil || !strings.Contains(err.Error(), "exited before connecting")) {
				t.Fatalf("a clean helper exit did not close its stream explicitly: %v", err)
			}
		})
	}
}

func TestLimaContainerdPendingHelperHonorsCancellation(t *testing.T) {
	fakeLimaContainerdStdio(t, "stall")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	connection, err := dialLimaContainerd(ctx, "synthetic-engine", "/run/synthetic/containerd.sock")
	if connection != nil {
		_ = connection.Close()
		t.Fatal("cancelled helper setup returned a connection")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending helper cancellation = %v", err)
	}
}

func fakeLimaContainerdStdio(t *testing.T, mode string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("native Windows subprocess ownership is covered by the platform-neutral stdio tests")
	}
	directory := t.TempDir()
	for name, script := range map[string]string{
		"limactl": "#!/bin/sh\nexec \"$PORTO_TEST_LIMA_EXECUTABLE\" -test.run=^TestLimaContainerdStdioHelperProcess$ -- \"$@\"\n",
		"ssh":     "#!/bin/sh\nprintf 'unexpected independent SSH forwarding attempt\\n' >&2\nexit 9\n",
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_TEST_LIMA_EXECUTABLE", os.Args[0])
	t.Setenv("PORTO_TEST_LIMA_STDIO", mode)
}

func TestLimaContainerdStdioHelperProcess(t *testing.T) {
	mode := os.Getenv("PORTO_TEST_LIMA_STDIO")
	if mode == "" {
		return
	}
	start := slices.Index(os.Args, "--")
	expected := []string{
		"shell", "--workdir=/", "synthetic-engine", "--", "sh", "-c",
		`exec "$HOME/.local/bin/porto-runtime-helper" dial-stdio "$1"`,
		"porto-containerd", "/run/synthetic/containerd.sock",
	}
	if start < 0 || !reflect.DeepEqual(os.Args[start+1:], expected) {
		fmt.Fprintln(os.Stderr, "invalid Lima helper channel arguments")
		os.Exit(2)
	}
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "synthetic runtime socket unavailable")
		os.Exit(7)
	case "exit":
		os.Exit(0)
	case "stall":
		time.Sleep(time.Hour)
		os.Exit(0)
	case "echo":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		os.Exit(2)
	}
}

func assertLimaContainerdEcho(t *testing.T, connection io.ReadWriter) {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		if _, err := connection.Write([]byte("ping")); err != nil {
			result <- err
			return
		}
		payload := make([]byte, 4)
		_, err := io.ReadFull(connection, payload)
		if err == nil && string(payload) != "ping" {
			err = fmt.Errorf("unexpected channel response %q", payload)
		}
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("established containerd channel stopped responding")
	}
}

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
	assertLimaContainerdEcho(t, connection)
	if err := connection.Close(); err != nil {
		t.Fatalf("close containerd tunnel: %v", err)
	}
	select {
	case <-connection.(*commandConn).done:
	default:
		t.Fatal("closing the tunnel did not reap its subprocess")
	}
}
