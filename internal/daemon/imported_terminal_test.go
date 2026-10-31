package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"k8s.io/client-go/tools/remotecommand"
)

type remoteExecutorFunc func(context.Context, remotecommand.StreamOptions) error

func (f remoteExecutorFunc) Stream(options remotecommand.StreamOptions) error {
	return f(context.Background(), options)
}
func (f remoteExecutorFunc) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return f(ctx, options)
}

func TestImportedTerminalProcessClosesPipesAndResizeQueueOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started, resized := make(chan struct{}), make(chan struct{})
	process := startKubernetesRemoteProcess(ctx, remoteExecutorFunc(func(ctx context.Context, options remotecommand.StreamOptions) error {
		initial := options.TerminalSizeQueue.Next()
		if initial == nil || initial.Width != 120 || initial.Height != 32 {
			t.Error("initial terminal dimensions missing")
		}
		if _, err := options.Stdout.Write([]byte("synthetic imported terminal")); err != nil {
			return err
		}
		close(started)
		next := options.TerminalSizeQueue.Next()
		if next == nil || next.Width != 132 || next.Height != 41 {
			t.Error("selected terminal resize missing")
		}
		close(resized)
		<-ctx.Done()
		return ctx.Err()
	}))
	output := make(chan string, 1)
	go func() { data, _ := io.ReadAll(process.Stdout()); output <- string(data) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("imported terminal did not start")
	}
	if err := process.Resize(context.Background(), 132, 41); err != nil {
		t.Fatal(err)
	}
	select {
	case <-resized:
	case <-time.After(3 * time.Second):
		t.Fatal("terminal resize did not reach selected stream")
	}
	cancel()
	if err := process.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("terminal cancellation: %v", err)
	}
	select {
	case text := <-output:
		if text != "synthetic imported terminal" {
			t.Fatalf("terminal output lost: %q", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled output pipe retained a handle")
	}
	if _, err := process.Stdin().Write([]byte("stale input")); err == nil {
		t.Fatal("canceled input remained writable")
	}
	if process.Next() != nil {
		t.Fatal("canceled resize queue retained a handle")
	}
}

func TestPTYRequestCancellationReapsItsCommandWithoutClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commands := make(chan *exec.Cmd, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		bridgePTYTerminal(w, r.WithContext(ctx), func(ctx context.Context) (*exec.Cmd, error) {
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHostPTYSubprocess$")
			command.Env = append(os.Environ(), "PORTO_TEST_PTY_CHILD=wait")
			commands <- command
			return command, nil
		}, "synthetic terminal failed")
	}))
	defer server.Close()
	readCtx, stop := context.WithTimeout(context.Background(), 8*time.Second)
	defer stop()
	conn, _, err := websocket.Dial(readCtx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for {
		_, data, err := conn.Read(readCtx)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "synthetic PTY output") {
			break
		}
	}
	command := <-commands
	cancel()
	select {
	case <-done:
		if command.ProcessState == nil {
			t.Fatal("request revocation did not reap its terminal")
		}
	case <-readCtx.Done():
		t.Fatal("revoked request retained a terminal while client remained connected")
	}
}
