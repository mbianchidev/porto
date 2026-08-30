package compose

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestCheckUsesDockerServerInfo(t *testing.T) {
	runner := &fakeRunner{output: []byte("28.3.2\n")}
	integration := newIntegration(runner, time.Second)

	if err := integration.Check(context.Background()); err != nil {
		t.Fatalf("Check: %v", err)
	}

	want := Command{
		Name: "docker",
		Args: []string{"info", "--format", "{{.ServerVersion}}"},
	}
	if !reflect.DeepEqual(runner.commands, []Command{want}) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, []Command{want})
	}
}

func TestCheckReportsActionableDaemonFailure(t *testing.T) {
	runner := &fakeRunner{
		output: []byte("failed to connect to the docker API at unix:///missing/docker.sock"),
		err:    errors.New("exit status 1"),
	}

	err := newIntegration(runner, time.Second).Check(context.Background())
	if !errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("error = %v, want ErrDaemonUnavailable", err)
	}
	for _, want := range []string{"start or repair the configured Docker-compatible runtime", "unix:///missing/docker.sock"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestCheckDistinguishesMissingDockerCLI(t *testing.T) {
	runner := &fakeRunner{err: &exec.Error{Name: "docker", Err: exec.ErrNotFound}}

	err := newIntegration(runner, time.Second).Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Porto daemon PATH") {
		t.Fatalf("error = %v", err)
	}
	if errors.Is(err, ErrDaemonUnavailable) {
		t.Fatalf("missing CLI error was classified as daemon unavailable: %v", err)
	}
}

func TestProjectNameIsStableAndInstanceSafe(t *testing.T) {
	if got := ProjectName(app.Project{ID: 42, Name: "Web App"}); got != "porto-42" {
		t.Fatalf("ProjectName with ID = %q", got)
	}
	if got := ProjectName(app.Project{Name: "Web App!"}); got != "porto-web-app" {
		t.Fatalf("ProjectName fallback = %q", got)
	}
}

func TestCheckTimesOut(t *testing.T) {
	runner := &fakeRunner{
		run: func(ctx context.Context, _ Command) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	integration := newIntegration(runner, time.Second)
	integration.checkTimeout = time.Millisecond

	err := integration.Check(context.Background())
	if !errors.Is(err, ErrDaemonUnavailable) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
}

func TestPublishedPortsParsesWarningsAndPrioritizesFrontend(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{output: []byte(`time="now" level=warning msg="version is obsolete"
{"Service":"backend","State":"running","Publishers":[{"PublishedPort":8080,"Protocol":"tcp"},{"PublishedPort":8080,"Protocol":"tcp"},{"PublishedPort":0,"Protocol":"tcp"}]}
{"Service":"frontend","State":"running","Publishers":[{"PublishedPort":3000,"Protocol":"tcp"},{"PublishedPort":3000,"Protocol":"tcp"},{"PublishedPort":5353,"Protocol":"udp"}]}
`)}
	project := app.Project{
		Name:     "web",
		Path:     root,
		Strategy: "compose",
		Command:  UpCommand("compose.yaml"),
		Port:     41007,
	}

	got, err := newIntegration(runner, time.Second).PublishedPorts(context.Background(), project)
	if err != nil {
		t.Fatalf("PublishedPorts: %v", err)
	}
	want := []PublishedPort{
		{Service: "frontend", Port: 3000},
		{Service: "backend", Port: 8080},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
	}
	wantCommand := Command{
		Dir:  root,
		Name: "docker",
		Args: []string{"compose", "-f", "compose.yaml", "ps", "--format", "json"},
		Env:  []string{"PORT=41007", "PORTO_PORT=41007", "COMPOSE_PROJECT_NAME=porto-web"},
	}
	if !reflect.DeepEqual(runner.commands, []Command{wantCommand}) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, []Command{wantCommand})
	}
}

func TestPublishedPortsParsesJSONArray(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{output: []byte(`warning
[{"Service":"web","State":"running","Publishers":[{"PublishedPort":4173,"Protocol":"tcp"}]}]
`)}

	got, err := newIntegration(runner, time.Second).PublishedPorts(context.Background(), app.Project{
		Name:    "web",
		Path:    root,
		Command: UpCommand("compose.yaml"),
	})
	if err != nil {
		t.Fatalf("PublishedPorts: %v", err)
	}
	want := []PublishedPort{{Service: "web", Port: 4173}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ports = %#v, want %#v", got, want)
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
		Env:  []string{"PORT=41007", "PORTO_PORT=41007", "COMPOSE_PROJECT_NAME=porto-web"},
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
