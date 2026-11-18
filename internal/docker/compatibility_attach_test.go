package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

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
