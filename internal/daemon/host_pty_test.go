package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHostPTYSubprocess(t *testing.T) {
	mode := os.Getenv("PORTO_TEST_PTY_CHILD")
	if mode != "1" && mode != "wait" {
		return
	}
	fmt.Fprintln(os.Stdout, "synthetic PTY output")
	if mode == "wait" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	os.Exit(7)
}

func TestPTYBridgeDisconnectReapsOnlyItsCommand(t *testing.T) {
	commands := make(chan *exec.Cmd, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostPTYSubprocess$")
			command.Env = append(os.Environ(), "PORTO_TEST_PTY_CHILD=wait")
			commands <- command
			return command, nil
		}, "synthetic terminal failed")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	var output strings.Builder
	for !strings.Contains(output.String(), "synthetic PTY output") {
		_, data, err := connection.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		output.Write(data)
	}
	command := <-commands
	_ = connection.CloseNow()
	select {
	case <-done:
		if command.ProcessState == nil {
			t.Fatal("terminal command was not reaped on disconnect")
		}
	case <-ctx.Done():
		t.Fatal("terminal disconnect did not close and reap its command")
	}
}

func TestPTYBridgeReportsExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostPTYSubprocess$")
			command.Env = append(os.Environ(), "PORTO_TEST_PTY_CHILD=1")
			return command, nil
		}, "synthetic terminal failed")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	for {
		_, _, err := connection.Read(ctx)
		if err == nil {
			continue
		}
		var closed websocket.CloseError
		if !errors.As(err, &closed) || !strings.Contains(closed.Reason, "exit code 7") {
			t.Fatalf("terminal close = %v; expected the subprocess exit code", err)
		}
		return
	}
}

func TestHostPTYPreservesOutputResizeAndExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostPTYSubprocess$")
	command.Env = append(os.Environ(), "PORTO_TEST_PTY_CHILD=1")
	terminal, err := startHostPTY(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	if err := terminal.Resize(132, 41); err != nil {
		t.Fatal(err)
	}
	output := make(chan string, 1)
	go func() {
		var buffer strings.Builder
		_, _ = io.Copy(&buffer, terminal)
		output <- buffer.String()
	}()
	err = terminal.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("PTY exit = %v, want exit 7", err)
	}
	_ = terminal.Close()
	select {
	case text := <-output:
		if !strings.Contains(text, "synthetic PTY output") {
			t.Fatalf("PTY output = %q", text)
		}
	case <-ctx.Done():
		t.Fatal("PTY output did not close after process exit")
	}
}
