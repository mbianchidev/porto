package daemon

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type sdkTerminalExitError struct{ code int }

func (e *sdkTerminalExitError) Error() string { return "synthetic SDK terminal exit" }
func (e *sdkTerminalExitError) ExitCode() int { return e.code }

type sdkTerminalReader struct {
	*io.PipeReader
	drained atomic.Bool
}

func (r *sdkTerminalReader) Read(data []byte) (int, error) {
	n, err := r.PipeReader.Read(data)
	if errors.Is(err, io.EOF) {
		r.drained.Store(true)
	}
	return n, err
}

type sdkTerminalProcess struct {
	inputReader  *io.PipeReader
	input        *io.PipeWriter
	outputWriter *io.PipeWriter
	errorWriter  *io.PipeWriter
	output       *sdkTerminalReader
	errors       *sdkTerminalReader
	commands     chan string
	sizes        chan [2]uint32
	done         chan struct{}
	once         sync.Once
	waitErr      error
	waited       atomic.Bool
	waitedEarly  atomic.Bool
	killed       atomic.Int32
}

func newSDKTerminalProcess() *sdkTerminalProcess {
	in, input := io.Pipe()
	out, output := io.Pipe()
	errs, errorOutput := io.Pipe()
	p := &sdkTerminalProcess{
		inputReader: in, input: input, outputWriter: output, errorWriter: errorOutput,
		output: &sdkTerminalReader{PipeReader: out}, errors: &sdkTerminalReader{PipeReader: errs},
		commands: make(chan string, 4), sizes: make(chan [2]uint32, 4), done: make(chan struct{}),
	}
	go func() {
		if _, err := io.WriteString(output, "sdk-terminal-ready\r\n"); err != nil {
			return
		}
		if _, err := io.WriteString(errorOutput, "sdk-terminal-stderr\r\n"); err != nil {
			return
		}
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			command := strings.TrimSpace(scanner.Text())
			p.commands <- command
			if command == "exit" {
				_, _ = io.WriteString(output, "sdk-terminal-final-output\r\n")
				p.finish(&sdkTerminalExitError{code: 37})
				return
			}
		}
	}()
	return p
}

func (p *sdkTerminalProcess) finish(err error) {
	p.once.Do(func() {
		p.waitErr = err
		_ = p.inputReader.Close()
		_ = p.outputWriter.Close()
		_ = p.errorWriter.Close()
		close(p.done)
	})
}

func (p *sdkTerminalProcess) Stdin() io.WriteCloser { return p.input }
func (p *sdkTerminalProcess) Stdout() io.ReadCloser { return p.output }
func (p *sdkTerminalProcess) Stderr() io.ReadCloser { return p.errors }
func (p *sdkTerminalProcess) PID() int              { return 1234 }
func (p *sdkTerminalProcess) Kill() error {
	p.killed.Add(1)
	p.finish(context.Canceled)
	return nil
}
func (p *sdkTerminalProcess) Resize(_ context.Context, width, height uint32) error {
	p.sizes <- [2]uint32{width, height}
	return nil
}
func (p *sdkTerminalProcess) Wait() error {
	p.waitedEarly.Store(!p.output.drained.Load() || !p.errors.drained.Load())
	<-p.done
	p.waited.Store(true)
	return p.waitErr
}

func sdkTerminalTestServer(t *testing.T, ctx context.Context, p *sdkTerminalProcess) (string, <-chan struct{}) {
	t.Helper()
	done := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		bridgeProcessTerminal(w, r, func(context.Context) (runtimes.Process, error) { return p, nil }, "container terminal failed")
	}))
	server.Config.BaseContext = func(net.Listener) context.Context { return ctx }
	server.Start()
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = p.Kill() })
	return "ws" + strings.TrimPrefix(server.URL, "http"), done
}

func readSDKTerminalUntil(t *testing.T, ctx context.Context, connection *websocket.Conn, want string) string {
	t.Helper()
	var output strings.Builder
	for !strings.Contains(output.String(), want) {
		kind, data, err := connection.Read(ctx)
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("terminal output before %q: kind=%v data=%q error=%v", want, kind, data, err)
		}
		output.Write(data)
	}
	return output.String()
}

func TestNativeWindowsContainersTerminalWebSocketPreservesSDKIOResizeAndExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := newSDKTerminalProcess()
	address, done := sdkTerminalTestServer(t, ctx, p)
	connection, _, err := websocket.Dial(ctx, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	output := readSDKTerminalUntil(t, ctx, connection, "sdk-terminal-stderr")
	if err := connection.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":41}`)); err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageBinary, []byte("synthetic input\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-p.sizes:
		if size != [2]uint32{132, 41} {
			t.Fatalf("SDK terminal size = %v", size)
		}
	case <-ctx.Done():
		t.Fatal("resize was not forwarded to the SDK process")
	}
	select {
	case input := <-p.commands:
		if input != "synthetic input" {
			t.Fatalf("SDK terminal input = %q", input)
		}
	case <-ctx.Done():
		t.Fatal("binary input was not forwarded to the SDK process")
	}
	if err := connection.Write(ctx, websocket.MessageBinary, []byte("exit\r\n")); err != nil {
		t.Fatal(err)
	}
	for {
		kind, data, err := connection.Read(ctx)
		if err != nil {
			var closeError websocket.CloseError
			if !errors.As(err, &closeError) || closeError.Code != websocket.StatusInternalError ||
				closeError.Reason != "terminal exited with exit code 37" {
				t.Fatalf("SDK terminal close lost authoritative exit 37: %v", err)
			}
			break
		}
		if kind != websocket.MessageBinary {
			t.Fatalf("terminal output frame is not binary: %v", kind)
		}
		output += string(data)
	}
	<-done
	if !strings.Contains(output, "sdk-terminal-final-output") || !p.waited.Load() || p.waitedEarly.Load() || p.killed.Load() != 0 {
		t.Fatalf("terminal did not drain and reap its SDK exec: output=%q waited=%v early=%v kills=%d",
			output, p.waited.Load(), p.waitedEarly.Load(), p.killed.Load())
	}
}

func TestNativeWindowsContainersTerminalDisconnectAndCancellationReapExec(t *testing.T) {
	for _, interruption := range []string{"disconnect", "request cancellation", "invalid resize"} {
		t.Run(interruption, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lifecycle, stop := context.WithCancel(ctx)
			defer stop()
			p := newSDKTerminalProcess()
			address, done := sdkTerminalTestServer(t, lifecycle, p)
			connection, _, err := websocket.Dial(ctx, address, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.CloseNow()
			readSDKTerminalUntil(t, ctx, connection, "sdk-terminal-stderr")
			switch interruption {
			case "disconnect":
				_ = connection.CloseNow()
			case "request cancellation":
				stop()
			case "invalid resize":
				if err := connection.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":0,"rows":41}`)); err != nil {
					t.Fatal(err)
				}
			}
			if interruption != "disconnect" {
				for err == nil {
					_, _, err = connection.Read(ctx)
				}
				if interruption == "invalid resize" && websocket.CloseStatus(err) != websocket.StatusInternalError {
					t.Fatalf("invalid resize did not fail the terminal session: %v", err)
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("interrupted WebSocket handler did not reap its SDK exec")
			}
			if p.killed.Load() == 0 || !p.waited.Load() {
				t.Fatalf("SDK exec survived interruption: kills=%d waited=%v", p.killed.Load(), p.waited.Load())
			}
			if interruption == "invalid resize" && len(p.sizes) != 0 {
				t.Fatal("invalid dimensions reached the native SDK")
			}
		})
	}
}
