package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/porto-containers/porto/internal/runtimes"
)

type inputForwardingOperations struct {
	execOperations
	received chan string
	fifo     string
}

func (o *inputForwardingOperations) ForwardTaskInput(_ context.Context, id string, input io.Reader, options taskInputOptions) error {
	data, err := io.ReadAll(input)
	o.received <- fmt.Sprintf("%s:%s await=%t close=%t", id, data, options.awaitTask, options.closeOnEOF)
	return err
}

func (o *inputForwardingOperations) TaskInput(context.Context, string) (string, error) {
	return o.fifo, nil
}

func TestStartAttachFollowsLogsAndForwardsInputForCompatibilityContainers(t *testing.T) {
	inspected := `{"Id":"demo","State":{"Running":true},"Config":{"Tty":false,"OpenStdin":true}}`
	var started []string
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "container inspect demo" {
			return []byte(inspected), nil
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		started = append(started, strings.Join(command.Args, " "))
		return newFakeProcess(func([]byte) ([]byte, []byte, error) { return nil, nil, nil }), nil
	}
	operations := &inputForwardingOperations{received: make(chan string, 1)}
	manager := New(runner)
	manager.execConnector = func(context.Context) (execOperations, error) {
		return borrowedExecOperations{operations}, nil
	}
	attach := func(stdin bool) error {
		process, err := manager.StartAttach(context.Background(), "demo", stdin)
		if err != nil {
			return err
		}
		if _, err := process.Stdin().Write([]byte("input")); err != nil && stdin {
			return err
		}
		_ = process.Stdin().Close()
		return process.Wait()
	}

	if err := attach(true); !errors.Is(err, errTaskInputClosed) {
		t.Fatalf("input to a task without stdin = %v, want an explicit refusal", err)
	}
	operations.fifo = "/run/containerd/fifo/1/demo-stdin"
	if err := attach(true); err != nil {
		t.Fatal(err)
	}
	if got := <-operations.received; got != "demo:input await=false close=false" {
		t.Fatalf("forwarded input = %q, want input that leaves a -d -i container's stdin open", got)
	}
	inspected = `{"Id":"demo","State":{"Running":true},"Config":{"Tty":false,"Labels":{"io.porto.container.managed":"1"}}}`
	if err := attach(false); err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || !strings.HasPrefix(started[0], "logs --follow --since ") || !strings.HasSuffix(started[0], " demo") ||
		started[1] != "attach --no-stdin demo" {
		t.Fatalf("started commands = %q", started)
	}
}

func TestNerdctlAttachDropsOnlyLoggingBinaryWarningsFromContainerStderr(t *testing.T) {
	input := "synthetic container error\n" +
		`time="2026-01-02T03:04:05Z" level=warning msg="writing container output to the logging binary failed; further output will not be logged" error="write |1: broken pipe"` + "\n" +
		`time="2026-01-02T03:04:05Z" level=fatal msg="synthetic start failure"` + "\n" +
		`time="synthetic partial line`
	var output strings.Builder
	if err := copyWithoutNerdctlLoggingWarnings(&output, iotest.OneByteReader(strings.NewReader(input))); err != nil {
		t.Fatal(err)
	}
	want := "synthetic container error\n" +
		`time="2026-01-02T03:04:05Z" level=fatal msg="synthetic start failure"` + "\n" +
		`time="synthetic partial line`
	if output.String() != want {
		t.Fatalf("filtered stderr = %q, want %q", output.String(), want)
	}
}

func TestNerdctlAttachStreamsPartialStderrLinesImmediately(t *testing.T) {
	source, writer := io.Pipe()
	process := withoutNerdctlLoggingWarnings(&fakeProcess{stderr: source})
	go func() { _, _ = writer.Write([]byte("Synthetic prompt: ")) }()
	read := make(chan string, 1)
	go func() {
		buffer := make([]byte, 64)
		count, _ := process.Stderr().Read(buffer)
		read <- string(buffer[:count])
	}()
	select {
	case got := <-read:
		if got != "Synthetic prompt: " {
			t.Fatalf("partial stderr = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("partial stderr line was held back")
	}
	_ = writer.Close()
	if rest, err := io.ReadAll(process.Stderr()); err != nil || len(rest) != 0 {
		t.Fatalf("remaining stderr = %q, %v", rest, err)
	}
	if err := process.Stderr().Close(); err != nil {
		t.Fatal(err)
	}
}
