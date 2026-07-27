package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

type fakeRunner struct {
	commands []Command
	output   []byte
	err      error
	run      func(context.Context, Command) ([]byte, error)
}

func (f *fakeRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	f.commands = append(f.commands, command)
	if f.run != nil {
		return f.run(ctx, command)
	}
	return f.output, f.err
}

func TestFindFileUsesDiscoveryPriority(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"compose.yaml", "docker-compose.yml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, ok := FindFile(root)
	if !ok || got != "docker-compose.yml" {
		t.Fatalf("FindFile() = %q, %t; want docker-compose.yml, true", got, ok)
	}
}

func TestDownUsesStartedConfigAndPortEnvironment(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"docker-compose.yml", "compose.yaml"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("services: {}\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	runner := &fakeRunner{}
	integration := newIntegration(runner, time.Second)
	project := app.Project{
		Name:     "web",
		Path:     root,
		Strategy: "compose",
		Command:  UpCommand("compose.yaml"),
		Port:     41007,
	}

	if err := integration.Down(context.Background(), project); err != nil {
		t.Fatalf("Down: %v", err)
	}

	want := Command{
		Dir:  root,
		Name: "docker",
		Args: []string{"compose", "-f", "compose.yaml", "down", "--remove-orphans"},
		Env:  []string{"PORT=41007", "PORTO_PORT=41007"},
	}
	if !reflect.DeepEqual(runner.commands, []Command{want}) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, []Command{want})
	}
}

func TestDownFallsBackToDetectedConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	runner := &fakeRunner{}

	err := newIntegration(runner, time.Second).Down(context.Background(), app.Project{
		Name:     "web",
		Path:     root,
		Strategy: "compose",
	})
	if err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := runner.commands[0].Args[2]; got != "compose.yml" {
		t.Fatalf("compose file = %q, want compose.yml", got)
	}
}

func TestDownIncludesCommandOutputInErrors(t *testing.T) {
	runner := &fakeRunner{output: []byte("daemon unavailable"), err: errors.New("exit status 1")}
	integration := newIntegration(runner, time.Second)

	err := integration.Down(context.Background(), app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  UpCommand("compose.yaml"),
	})
	if err == nil || !strings.Contains(err.Error(), "daemon unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestDownTimesOut(t *testing.T) {
	runner := &fakeRunner{
		run: func(ctx context.Context, _ Command) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	integration := newIntegration(runner, time.Millisecond)

	err := integration.Down(context.Background(), app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  UpCommand("compose.yaml"),
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
}
