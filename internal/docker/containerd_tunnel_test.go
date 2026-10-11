package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestLimaContainerdSSHTunnelSurvivesDialCancellation(t *testing.T) {
	fakeLimaContainerdSSH(t, "echo")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection, err := dialLimaContainerd(ctx, "test-engine", "synthetic-config", "/run/test/containerd.sock")
	if err != nil {
		t.Fatalf("connect a foreground containerd SSH tunnel: %v", err)
	}
	defer connection.Close()

	cancel()
	if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("dial cancellation closed the established SSH tunnel: %v", err)
	}
	if string(response) != "ping" {
		t.Fatalf("SSH tunnel response = %q, want ping", response)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close containerd SSH tunnel: %v", err)
	}
	tunnel := connection.(*limaContainerdConn)
	if tunnel.command.ProcessState == nil {
		t.Fatal("closing the SSH tunnel did not reap its subprocess")
	}
	if _, err := os.Stat(tunnel.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closing the SSH tunnel left its socket directory: %v", err)
	}
}

func TestLimaContainerdSSHTunnelReportsCleanExit(t *testing.T) {
	directory := fakeLimaContainerdSSH(t, "exit")
	t.Setenv("TMPDIR", directory)
	_, err := dialLimaContainerd(context.Background(), "test-engine", "synthetic-config", "/run/test/containerd.sock")
	if err == nil || !strings.Contains(err.Error(), "SSH exited with status 0") {
		t.Fatalf("clean SSH exit lost its diagnostic: %v", err)
	}
	assertNoLimaContainerdTunnelFiles(t, directory)
}

func TestLimaContainerdSSHTunnelReportsSubprocessFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		mode    string
		message string
	}{
		{"stderr", "fail", "synthetic SSH forwarding failure"},
		{"exit status", "fail-silent", "exit status 7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := fakeLimaContainerdSSH(t, test.mode)
			t.Setenv("TMPDIR", directory)
			_, err := dialLimaContainerd(context.Background(), "test-engine", "synthetic-config", "/run/test/containerd.sock")
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("SSH tunnel lost its subprocess failure: %v", err)
			}
			assertNoLimaContainerdTunnelFiles(t, directory)
		})
	}
}

func TestLimaContainerdSSHTunnelCancellationCleansPendingConnection(t *testing.T) {
	directory := fakeLimaContainerdSSH(t, "stall")
	t.Setenv("TMPDIR", directory)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := dialLimaContainerd(ctx, "test-engine", "synthetic-config", "/run/test/containerd.sock")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending SSH tunnel cancellation = %v, want deadline exceeded", err)
	}
	assertNoLimaContainerdTunnelFiles(t, directory)
}

func assertNoLimaContainerdTunnelFiles(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "ssh" {
		t.Fatalf("SSH tunnel left temporary files: %v", entries)
	}
}

func fakeLimaContainerdSSH(t *testing.T, mode string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the Lima stdio tunnel")
	}
	directory := t.TempDir()
	script := "#!/bin/sh\nexec \"$PORTO_TEST_SSH_EXECUTABLE\" -test.run=^TestLimaContainerdSSHHelperProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(directory, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_TEST_SSH_EXECUTABLE", os.Args[0])
	t.Setenv("PORTO_TEST_LIMA_SSH", mode)
	return directory
}

func TestLimaContainerdSSHHelperProcess(t *testing.T) {
	switch os.Getenv("PORTO_TEST_LIMA_SSH") {
	case "":
		return
	case "exit":
		os.Exit(0)
	case "fail":
		fmt.Fprintln(os.Stderr, "synthetic SSH forwarding failure")
		os.Exit(7)
	case "fail-silent":
		os.Exit(7)
	case "stall":
		time.Sleep(time.Hour)
		os.Exit(0)
	case "echo":
	default:
		fmt.Fprintln(os.Stderr, "unknown synthetic SSH mode")
		os.Exit(1)
	}
	options := map[string]string{}
	var forwarding string
	for index := 0; index+1 < len(os.Args); index++ {
		switch os.Args[index] {
		case "-o":
			option := strings.SplitN(os.Args[index+1], "=", 2)
			if len(option) == 2 {
				options[option[0]] = option[1]
			}
		case "-S":
			options["ControlPath"] = os.Args[index+1]
		case "-L":
			forwarding = os.Args[index+1]
		}
	}
	for option, value := range map[string]string{
		"ControlMaster":           "no",
		"ControlPath":             "none",
		"ControlPersist":          "no",
		"ForkAfterAuthentication": "no",
	} {
		if options[option] != value {
			// Lima's inherited multiplexing can exit successfully after forwarding.
			os.Exit(0)
		}
	}
	localSocket, _, ok := strings.Cut(forwarding, ":")
	if !ok {
		fmt.Fprintln(os.Stderr, "synthetic SSH forwarding address is missing")
		os.Exit(1)
	}
	listener, err := net.Listen("unix", localSocket)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	connection, err := listener.Accept()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := io.Copy(connection, connection); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = connection.Close()
	_ = listener.Close()
	os.Exit(0)
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
