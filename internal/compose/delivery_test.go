package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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

func TestDeliveryReadinessChecksEveryEnabledStackReplica(t *testing.T) {
	for _, test := range []struct {
		name          string
		configuration string
		processes     string
		wantError     bool
	}{
		{
			name:          "all services and replicas ready with a completed dependency",
			configuration: `{"services":{"web":{"deploy":{"replicas":2},"depends_on":{"migrate":{"condition":"service_completed_successfully"}}},"worker":{"scale":2},"migrate":{},"disabled":{"deploy":{"replicas":0}},"optional":{"profiles":["tools"]}}}`,
			processes:     `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"web","State":"running","Health":"healthy"},{"Service":"worker","State":"running"},{"Service":"worker","State":"running"},{"Service":"migrate","State":"exited","ExitCode":0}]`,
		},
		{
			name:          "one ready replica does not stand in for the whole service",
			configuration: `{"services":{"web":{"deploy":{"replicas":2}},"worker":{}}}`,
			processes:     `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"worker","State":"running"}]`,
			wantError:     true,
		},
		{
			name:          "an unhealthy replica fails the whole stack",
			configuration: `{"services":{"web":{"deploy":{"replicas":2}}}}`,
			processes:     `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"web","State":"running","Health":"unhealthy"}]`,
			wantError:     true,
		},
		{
			name:          "a replica with pending health is not ready",
			configuration: `{"services":{"web":{"scale":2}}}`,
			processes:     `[{"Service":"web","State":"running","Health":"healthy"},{"Service":"web","State":"running","Health":"starting"}]`,
			wantError:     true,
		},
		{
			name:          "an intentionally scaled down stack is complete without containers",
			configuration: `{"services":{"worker":{"scale":0},"web":{"deploy":{"replicas":0}}}}`,
			processes:     `[]`,
		},
		{
			name:          "failed completion dependencies fail the stack",
			configuration: `{"services":{"web":{},"migrate":{}}}`,
			processes:     `[{"Service":"web","State":"running"},{"Service":"migrate","State":"exited","ExitCode":1}]`,
			wantError:     true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PORTO_HOME", t.TempDir())
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			runner := &fakeRunner{run: func(_ context.Context, command Command) ([]byte, error) {
				if slices.Contains(command.Args, "config") {
					return []byte(test.configuration), nil
				}
				if test.wantError {
					cancel()
				}
				return []byte(test.processes), nil
			}}
			integration := New(runner)
			project := app.Project{ID: 94, Name: "synthetic-stack", Path: t.TempDir(), Command: "docker compose -f compose.yaml up"}
			err := integration.WaitHealthy(ctx, project)
			if (err != nil) != test.wantError {
				t.Fatalf("stack readiness = %v, wantError = %v", err, test.wantError)
			}
		})
	}
}

func TestDeliveryReadinessDistinguishesCompletionJobsFromLongRunningServices(t *testing.T) {
	const completionStack = `{"services":{"web":{"depends_on":{"migrate":{"condition":"service_completed_successfully"}}},"migrate":{}}}`
	for _, test := range []struct {
		name          string
		configuration string
		processes     []string
		wantError     bool
	}{
		{
			name:          "a health-checked service cannot complete before passing health",
			configuration: `{"services":{"web":{"healthcheck":{"test":["CMD","true"]}}}}`,
			processes:     []string{`[{"Service":"web","State":"exited","Health":"starting","ExitCode":0}]`},
			wantError:     true,
		},
		{
			name:          "a long-running service does not become a job by exiting successfully",
			configuration: `{"services":{"web":{}}}`,
			processes:     []string{`[{"Service":"web","State":"exited","ExitCode":0}]`},
			wantError:     true,
		},
		{
			name:          "completion jobs must actually finish rather than just run",
			configuration: completionStack,
			processes: []string{
				`[{"Service":"web","State":"running"},{"Service":"migrate","State":"running"}]`,
				`[{"Service":"web","State":"running"},{"Service":"migrate","State":"exited","ExitCode":0}]`,
			},
		},
		{
			name:          "declared health must be observed before readiness",
			configuration: `{"services":{"web":{"healthcheck":{"test":["CMD","true"]}}}}`,
			processes: []string{
				`[{"Service":"web","State":"running"}]`,
				`[{"Service":"web","State":"running","Health":"healthy"}]`,
			},
		},
		{
			name:          "inactive consumers do not turn a running service into a completion job",
			configuration: `{"services":{"web":{},"optional":{"profiles":["tools"],"depends_on":{"web":{"condition":"service_completed_successfully"}}}}}`,
			processes:     []string{`[{"Service":"web","State":"exited","ExitCode":0}]`},
			wantError:     true,
		},
		{
			name:          "successful completion requires an explicitly observed exit code",
			configuration: completionStack,
			processes:     []string{`[{"Service":"web","State":"running"},{"Service":"migrate","State":"exited"}]`},
			wantError:     true,
		},
		{
			name:          "disabled healthchecks do not require a health result",
			configuration: `{"services":{"web":{"healthcheck":{"disable":true}}}}`,
			processes:     []string{`[{"Service":"web","State":"running"}]`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("PORTO_HOME", t.TempDir())
			observations := 0
			runner := &fakeRunner{run: func(_ context.Context, command Command) ([]byte, error) {
				if slices.Contains(command.Args, "config") {
					return []byte(test.configuration), nil
				}
				result := test.processes[min(observations, len(test.processes)-1)]
				observations++
				return []byte(result), nil
			}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := New(runner).WaitHealthy(ctx, app.Project{ID: 98, Name: "synthetic-intent", Path: t.TempDir(), Command: "docker compose -f compose.yaml up"})
			if (err != nil) != test.wantError {
				t.Fatalf("readiness = %v, wantError = %v", err, test.wantError)
			}
			if !test.wantError && observations < len(test.processes) {
				t.Fatalf("readiness was acknowledged before the required observation: %d of %d", observations, len(test.processes))
			}
		})
	}
}

func TestDeliveryPortPlanningExcludesInactiveStackServices(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	runner := &fakeRunner{run: func(_ context.Context, _ Command) ([]byte, error) {
		return []byte(`{"services":{"web":{"ports":[{"target":80,"published":"41001"}]},"api":{"ports":[{"target":8080,"published":"41002"}]},"optional":{"profiles":["tools"],"ports":[{"target":9000,"published":"41003"}]},"disabled":{"deploy":{"replicas":0},"ports":[{"target":9001,"published":"41004"}]}}}`), nil
	}}
	plan, err := New(runner).PreparePorts(context.Background(), app.Project{ID: 95, Name: "synthetic-stack", Path: t.TempDir(), Command: "docker compose -f compose.yaml up"}, map[int]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Ports) != 2 || plan.Ports[0].Service != "web" || plan.Ports[1].Service != "api" {
		t.Fatalf("inactive services received published ports: %+v", plan.Ports)
	}
}

func TestUpArgumentsReconcilesTheWholeOwnedStackWithoutDeletingVolumes(t *testing.T) {
	project := app.Project{ID: 97, Path: t.TempDir()}
	plan := PortPlan{ConfigFile: "synthetic-compose.json", OverridePath: "synthetic-ports.yaml"}
	args := UpArguments(project, plan, true)
	if !slices.Contains(args, "--remove-orphans") || !slices.Contains(args, "porto-97") {
		t.Fatalf("removed services would remain in the owned stack: %v", args)
	}
	for _, flag := range []string{"--volumes", "--renew-anon-volumes", "--force-recreate"} {
		if slices.Contains(args, flag) {
			t.Fatalf("stack reconciliation unexpectedly uses %s", flag)
		}
	}
	if slices.Contains(UpArguments(project, plan, false), "--remove-orphans") {
		t.Fatal("ordinary Compose startup unexpectedly opted into recorded-stack reconciliation")
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

func TestNativeComposeStackPreservesDependenciesStorageAndLiteralValues(t *testing.T) {
	if os.Getenv("PORTO_TEST_COMPOSE") != "1" {
		t.Skip("set PORTO_TEST_COMPOSE=1 for native multi-service Compose validation without an engine")
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	directory := filepath.Join(t.TempDir(), "Synthetic stack source")
	if err := os.MkdirAll(filepath.Join(directory, "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "settings.json"), []byte(`{"synthetic":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`services:
  web:
    image: registry.invalid/synthetic-web:one
    build:
      context: ./web
    environment:
      TOKEN: ${TOKEN:?Required}
    command: ["sh", "-c", "printf '%s' $$PATH"]
    depends_on:
      database:
        condition: service_healthy
    ports:
      - "8080:80"
    configs:
      - settings
    networks:
      - private
  database:
    image: registry.invalid/synthetic-db:one
    healthcheck:
      test: ["CMD", "true"]
    volumes:
      - stack-data:/data
    networks:
      - private
  worker:
    image: registry.invalid/synthetic-worker:one
    deploy:
      replicas: 2
    depends_on:
      database:
        condition: service_healthy
    networks:
      - private
volumes:
  stack-data: {}
networks:
  private:
    internal: true
configs:
  settings:
    file: ./settings.json
`)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	project := app.Project{ID: 96, Name: "synthetic-stack", Path: directory, Strategy: "compose"}
	integration := NewWithDockerHost(nil, "unix:///synthetic/not-a-real-engine.sock")
	first, err := integration.ValidateDefinition(ctx, project, directory, raw, map[string]string{"TOKEN": "synthetic$TOKEN${UNSET}"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := integration.ValidateDefinition(ctx, project, directory, first, nil)
	if err != nil {
		t.Fatalf("recorded stack could not be loaded again: %v", err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(first, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("loading the recorded Compose stack changed its configuration or interpolated literal values again")
	}
	services := before["services"].(map[string]any)
	if len(services) != 3 || len(before["volumes"].(map[string]any)) != 1 || len(before["networks"].(map[string]any)) != 1 {
		t.Fatal("stack services, named volumes or networks were lost")
	}
	web := services["web"].(map[string]any)
	if web["build"].(map[string]any)["context"] != filepath.Join(directory, "web") {
		t.Fatal("relative stack build context resolved outside the selected source")
	}
}

func TestComposeProfilesAreExplicitBoundedAndCanonical(t *testing.T) {
	for _, test := range []struct {
		name     string
		profiles []string
		want     []string
		invalid  bool
	}{
		{"default services", nil, []string{}, false},
		{"deduplicated sorted names", []string{"tools", "dev", "tools"}, []string{"dev", "tools"}, false},
		{"all profiles", []string{"tools", "*"}, []string{"*"}, false},
		{"flag is not a profile", []string{"--profile"}, nil, true},
		{"environment is not a profile", []string{"COMPOSE_PROFILES=tools"}, nil, true},
		{"empty explicit name", []string{""}, nil, true},
		{"overlong name", []string{strings.Repeat("a", 129)}, nil, true},
		{"bounded count", make([]string, 65), nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeProfiles(test.profiles)
			if (err != nil) != test.invalid || !slices.Equal(got, test.want) {
				t.Fatalf("profile normalization = %v, %v; want %v, invalid=%t", got, err, test.want, test.invalid)
			}
		})
	}
}

func TestNativeComposeProfilesFreezeExactlyTheSelectedServicesForRestartAndRollback(t *testing.T) {
	if os.Getenv("PORTO_TEST_COMPOSE") != "1" {
		t.Skip("set PORTO_TEST_COMPOSE=1 for native profile selection without an engine")
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv("COMPOSE_PROFILES", "debug")
	directory := t.TempDir()
	project := app.Project{ID: 99, Name: "synthetic-profiles", Path: directory, Strategy: "compose"}
	raw := []byte(`services:
  web:
    image: registry.invalid/synthetic-web:one
  tools:
    image: registry.invalid/synthetic-tools:one
    profiles: [tools]
    depends_on:
      web:
        condition: service_started
    deploy:
      replicas: 2
  debug:
    image: registry.invalid/synthetic-debug:one
    profiles: [debug]
`)
	for _, test := range []struct {
		name     string
		profiles []string
		services []string
	}{
		{"default ignores host profiles", nil, []string{"web"}},
		{"explicit tools", []string{"tools"}, []string{"web", "tools"}},
		{"explicit debug", []string{"debug"}, []string{"web", "debug"}},
		{"all declared profiles", []string{"*"}, []string{"web", "tools", "debug"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			integration := NewWithDockerHost(nil, "unix:///synthetic/not-a-real-engine.sock")
			effective, err := integration.ValidateDefinition(ctx, project, directory, raw, nil, test.profiles...)
			if err != nil {
				t.Fatal(err)
			}
			var selected struct {
				Services map[string]map[string]json.RawMessage `json:"services"`
			}
			if err := json.Unmarshal(effective, &selected); err != nil {
				t.Fatal(err)
			}
			if len(selected.Services) != len(test.services) {
				t.Fatalf("wrong active service count: %d, want %d", len(selected.Services), len(test.services))
			}
			for _, name := range test.services {
				service, found := selected.Services[name]
				if !found || service["profiles"] != nil {
					t.Fatalf("selected service %s was lost or still depends on host profile activation", name)
				}
			}
			reloaded, err := integration.ValidateDefinition(ctx, project, directory, effective, nil)
			if err != nil {
				t.Fatalf("recorded selection cannot restart without profile/environment inheritance: %v", err)
			}
			var before, after map[string]any
			if json.Unmarshal(effective, &before) != nil || json.Unmarshal(reloaded, &after) != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("restart or rollback changed the frozen selected service model")
			}
		})
	}
	if _, err := New(nil).ValidateDefinition(context.Background(), project, directory, raw, nil, "not-declared"); err == nil {
		t.Fatal("an unknown profile silently fell back to the default services")
	}
}
