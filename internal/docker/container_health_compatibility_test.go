package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestCompatibilityCreationPreservesExecFormHealthWithoutRequiringAShell(t *testing.T) {
	runner := &fakeRunner{}
	runner.handler = func(command runtimes.Command) ([]byte, error) { return []byte("synthetic-created\n"), nil }
	manager := New(runner)
	operations := &fakeContainerOperations{}
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	_, err := manager.CreateContainer(context.Background(), CreateContainerRequest{
		Image: "synthetic:latest", Healthcheck: &ContainerHealthcheck{Test: []string{"CMD", "/fixture", "argument with spaces"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range operations.calls {
		if strings.Contains(call, "update-labels synthetic-created") &&
			strings.Contains(call, `"test":["CMD","/fixture","argument with spaces"]`) {
			return
		}
	}
	t.Fatalf("exec-form health was silently downgraded to CMD-SHELL by the compatibility CLI: %v", operations.calls)
}

type rejectedHealthMetadata struct {
	fakeContainerOperations
	reject func() error
}

func (operations *rejectedHealthMetadata) UpdateLabels(context.Context, string, map[string]string) error {
	return operations.reject()
}

func TestExecHealthMetadataFailureCleansOnlyTheNewContainerAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	metadataFailure := errors.New("synthetic health metadata failure")
	cleaned := false
	runner := lifecycleDeadlineRunner(func(ctx context.Context, command runtimes.Command) ([]byte, error) {
		if len(command.Args) > 0 && command.Args[0] == "create" {
			return []byte("synthetic-created\n"), nil
		}
		if strings.Join(command.Args, " ") == "rm --force synthetic-created" && ctx.Err() == nil {
			cleaned = true
			return nil, nil
		}
		return nil, errors.New("unexpected or canceled cleanup command")
	})
	manager := New(runner)
	operations := &rejectedHealthMetadata{reject: func() error { cancel(); return metadataFailure }}
	manager.operationsConnector = func(context.Context) (containerOperations, error) { return operations, nil }
	id, err := manager.CreateContainer(ctx, CreateContainerRequest{
		Image: "synthetic:latest", Healthcheck: &ContainerHealthcheck{Test: []string{"CMD", "/fixture", "--selftest"}},
	})
	if id != "" || !errors.Is(err, metadataFailure) || !cleaned {
		t.Fatalf("failed health configuration leaked a partially created container: id=%q error=%v cleaned=%t", id, err, cleaned)
	}
}

func TestExecHealthWithoutMetadataAccessFailsBeforeCreatingAnything(t *testing.T) {
	runner := &fakeRunner{}
	manager := New(runner)
	_, err := manager.CreateContainer(context.Background(), CreateContainerRequest{
		Image: "synthetic:latest", Healthcheck: &ContainerHealthcheck{Test: []string{"CMD", "/fixture"}},
	})
	if !errors.Is(err, ErrUnsupported) || len(runner.commands) != 0 {
		t.Fatalf("unsupported exec health mutated the backend: commands=%v, error=%v", runner.commands, err)
	}
}
