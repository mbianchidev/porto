package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

func TestDeliveryReadinessUsesProjectScopedConfigAndReportsUnhealthyServices(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	project := app.Project{ID: 91, Name: "synthetic", Path: t.TempDir(), Strategy: "compose", Command: "docker compose -f compose.yaml up"}
	recorded := filepath.Join(t.TempDir(), "recorded.json")
	runner := &fakeRunner{run: func(_ context.Context, command Command) ([]byte, error) {
		if !slices.Contains(command.Args, recorded) || !slices.Contains(command.Args, project.Path) || !slices.Contains(command.Env, "COMPOSE_PROJECT_NAME=porto-91") {
			t.Fatalf("unscoped Compose delivery: %+v", command)
		}
		if slices.Contains(command.Args, "config") {
			return []byte(`{"services":{"app":{},"disabled":{"profiles":["optional"]}}}`), nil
		}
		return []byte(`[{"Service":"app","State":"running","Health":"unhealthy","ExitCode":0}]`), nil
	}}
	integration := NewWithDockerHost(runner, "unix:///synthetic/porto.sock")
	integration.SetDefinitionResolver(func(app.Project) (string, error) { return recorded, nil })
	if err := integration.WaitHealthy(context.Background(), project); err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Fatalf("unhealthy deployment appeared ready: %v", err)
	}
	if err := integration.StopServices(context.Background(), project); err != nil {
		t.Fatal(err)
	}
	for _, command := range runner.commands {
		if slices.Contains(command.Args, "down") || slices.Contains(command.Args, "--volumes") {
			t.Fatal("delivery cleanup deleted persistent resources")
		}
	}
}

func TestDeliveryPreservesRecordedPortOverridesAndPropagatesResolverFailures(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	project := app.Project{ID: 92, Name: "synthetic", Path: t.TempDir(), Strategy: "compose", Command: "docker compose -f compose.yaml up"}
	path, err := OverridePath(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePortOverride(path, []PlannedPort{{Service: "app", Target: 8080, Published: 41888, Protocol: "tcp"}}); err != nil {
		t.Fatal(err)
	}
	previous, err := priorPorts(project)
	if err != nil || previous["app:8080/tcp"] != 41888 {
		t.Fatalf("stable port was lost: %+v %v", previous, err)
	}
	integration := New(&fakeRunner{})
	integration.SetDefinitionResolver(func(app.Project) (string, error) { return "", errors.New("synthetic credential store locked") })
	if _, err := integration.PreparePorts(context.Background(), project, map[int]bool{}); err == nil {
		t.Fatal("missing encrypted definition silently fell back to another source")
	}
}

func TestNativeComposeDefinitionValidationIsLocalAndPreservesSecretValues(t *testing.T) {
	if os.Getenv("PORTO_TEST_COMPOSE") != "1" {
		t.Skip("set PORTO_TEST_COMPOSE=1 for native Compose config acceptance; no engine or network access is required")
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "Synthetic source with spaces")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	project := app.Project{ID: 93, Name: "synthetic", Path: root, Strategy: "compose"}
	raw := []byte("services:\n  app:\n    image: registry.invalid/synthetic:one\n    environment:\n      TOKEN: ${TOKEN:?Required secret variable}\n    ports:\n      - '127.0.0.1:8080:80'\n    cpus: 1\n    mem_limit: 256m\n    volumes:\n      - synthetic-data:/data\nvolumes:\n  synthetic-data: {}\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	integration := NewWithDockerHost(nil, "unix:///synthetic/not-a-real-engine.sock")
	effective, err := integration.ValidateDefinition(ctx, project, root, raw, map[string]string{"TOKEN": "synthetic-private-value"})
	if err != nil || !json.Valid(effective) || !bytes.Contains(effective, []byte("synthetic-private-value")) {
		t.Fatalf("native Compose config failed: %v", err)
	}
	if _, err := integration.ValidateDefinition(ctx, project, root, raw, nil); err == nil || strings.Contains(err.Error(), "synthetic-private-value") {
		t.Fatal("missing-variable validation succeeded or leaked a secret")
	}
}
