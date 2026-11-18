package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type taskOutputOperations struct {
	execOperations
	output string
}

func (o taskOutputOperations) TaskOutput(context.Context, string) (string, error) {
	return o.output, nil
}
func (taskOutputOperations) Close() error { return nil }

func TestStartAttachFollowsLogsOnlyForLogURITasks(t *testing.T) {
	var started []string
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") == "container inspect demo" {
			return []byte(`{"Id":"demo","State":{"Running":true},"Config":{"Tty":false,"OpenStdin":true}}`), nil
		}
		return nil, fmt.Errorf("unexpected command: %+v", command)
	}
	runner.starter = func(command runtimes.Command) (runtimes.Process, error) {
		started = append(started, strings.Join(command.Args, " "))
		return newFakeProcess(func([]byte) ([]byte, []byte, error) { return nil, nil, nil }), nil
	}
	manager := New(runner)
	attach := func(output string, stdin bool) error {
		manager.execConnector = func(context.Context) (execOperations, error) {
			// Production lends the inventory's runtime through this wrapper.
			return borrowedExecOperations{taskOutputOperations{output: output}}, nil
		}
		process, err := manager.StartAttach(context.Background(), "demo", stdin)
		if err == nil {
			_ = process.Stdin().Close()
			_ = process.Wait()
		}
		return err
	}

	if err := attach("binary:///usr/local/bin/nerdctl?_NERDCTL_INTERNAL_LOGGING=/synthetic", false); err != nil {
		t.Fatal(err)
	}
	if err := attach("binary:///usr/local/bin/nerdctl?_NERDCTL_INTERNAL_LOGGING=/synthetic", true); err == nil || !strings.Contains(err.Error(), "docker exec -i") {
		t.Fatalf("open input on a detached task = %v, want an explicit refusal", err)
	}
	if err := attach("/run/user/1000/containerd/fifo/1/demo-stdout", false); err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || !strings.HasPrefix(started[0], "logs --follow --since ") || !strings.HasSuffix(started[0], " demo") ||
		started[1] != "attach --no-stdin demo" {
		t.Fatalf("started commands = %q", started)
	}
}
