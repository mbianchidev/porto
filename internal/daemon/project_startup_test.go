package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	projectsetup "github.com/mbianchidev/porto/internal/setup"
)

type projectSetupRunnerFunc func(context.Context, app.Project, func(string, string) error) (projectsetup.Result, error)

func (f projectSetupRunnerFunc) Run(ctx context.Context, project app.Project, emit func(string, string) error) (projectsetup.Result, error) {
	return f(ctx, project, emit)
}

func TestStartProjectInstallsDependenciesBeforeLaunching(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_PROCESS", "1")
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_STATUS", strconv.Itoa(http.StatusOK))
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "package.json"), []byte(`{"scripts":{"dev":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     directory,
		Strategy: "package",
		Command:  "test -f dependencies.ready && " + daemonHTTPHelperCommand(),
	})
	started := make(chan struct{})
	release := make(chan struct{})
	server := New(st, nil)
	server.readinessDelay = 10 * time.Millisecond
	server.setupRunner = projectSetupRunnerFunc(func(ctx context.Context, project app.Project, emit func(string, string) error) (projectsetup.Result, error) {
		close(started)
		select {
		case <-ctx.Done():
			return projectsetup.Result{}, ctx.Err()
		case <-release:
		}
		if err := os.WriteFile(filepath.Join(project.Path, "dependencies.ready"), nil, 0o600); err != nil {
			return projectsetup.Result{}, err
		}
		return projectsetup.Result{Commands: []string{"npm install"}}, emit("stdout", "dependencies installed")
	})
	t.Cleanup(func() {
		if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
			t.Errorf("stop project: %v", err)
		}
	})

	project, err := server.startProject(context.Background(), project.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Run did not start dependency setup")
	}
	if project.Status != "starting" || project.PID != 0 {
		t.Fatalf("runtime during setup = %+v, want starting without a runtime process", project)
	}
	if _, err := os.Stat(filepath.Join(directory, "dependencies.ready")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dependencies were installed before setup was released: %v", err)
	}
	close(release)
	waitForProjectStatus(t, st, project.ID, "running")
	logs, err := st.Logs(context.Background(), project.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, entry := range logs {
		messages = append(messages, entry.Line)
	}
	if !strings.Contains(strings.Join(messages, "\n"), "Dependency setup completed.") {
		t.Fatalf("dependency setup logs = %v", messages)
	}
}

func TestStartProjectPreparesEverySupportedStack(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HELPER_PROCESS", "1")
	tests := []struct {
		name     string
		strategy string
		files    map[string]string
		want     []string
	}{
		{"make", "make", map[string]string{"Makefile": "deps:\n\t@echo ready\n"}, []string{"make deps"}},
		{"compose", "compose", map[string]string{"compose.yaml": "services: {}\n"}, []string{"docker compose -f compose.yaml build --no-cache"}},
		{"npm", "package", map[string]string{"package.json": "{}"}, []string{"npm install --include=dev"}},
		{"npm lock", "package", map[string]string{"package.json": "{}", "package-lock.json": "{}"}, []string{"npm ci --include=dev"}},
		{"pnpm", "package", map[string]string{"package.json": "{}", "pnpm-lock.yaml": ""}, []string{"pnpm install --frozen-lockfile --prod=false"}},
		{"yarn", "package", map[string]string{"package.json": "{}", "yarn.lock": ""}, []string{"yarn install --frozen-lockfile"}},
		{"bun", "package", map[string]string{"package.json": "{}", "bun.lock": ""}, []string{"bun install --frozen-lockfile"}},
		{"uv", "python", map[string]string{"pyproject.toml": "", "uv.lock": ""}, []string{"uv sync --frozen"}},
		{"poetry", "python", map[string]string{"pyproject.toml": "", "poetry.lock": ""}, []string{"poetry install --no-interaction"}},
		{"pipenv", "python", map[string]string{"pyproject.toml": "", "Pipfile.lock": ""}, []string{"pipenv sync"}},
		{"requirements", "python", map[string]string{"requirements.txt": ""}, []string{"python3 -m venv .venv", "VENVPYTHON -m pip install -r requirements.txt"}},
		{"pyproject", "python", map[string]string{"pyproject.toml": ""}, []string{"python3 -m venv .venv", "VENVPYTHON -m pip install -e ."}},
		{"go", "go", map[string]string{"go.mod": "module example.invalid/app\n"}, []string{"go mod download"}},
		{"rust", "rust", map[string]string{"Cargo.toml": "[package]\nname='synthetic'\n"}, []string{"cargo fetch"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			writeStartupFiles(t, directory, tt.files)
			st, project := testProject(t, app.Project{
				Name:     "synthetic",
				Path:     directory,
				Strategy: tt.strategy,
				Command:  daemonHelperCommand(),
			})
			server := New(st, nil)
			server.compose = &fakeComposeCleanup{}
			plans := make(chan []string, 1)
			server.setupRunner = projectSetupRunnerFunc(func(_ context.Context, project app.Project, _ func(string, string) error) (projectsetup.Result, error) {
				commands, err := projectsetup.Plan(project)
				var plan []string
				for _, command := range commands {
					if filepath.IsAbs(command.Name) {
						command.Name = "VENVPYTHON"
					}
					plan = append(plan, command.String())
				}
				plans <- plan
				return projectsetup.Result{Commands: plan}, err
			})
			t.Cleanup(func() {
				if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
					t.Errorf("stop project: %v", err)
				}
			})

			if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-plans:
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("setup commands = %v, want %v", got, tt.want)
				}
			case <-time.After(time.Second):
				t.Fatal("dependency setup was skipped")
			}
			waitForStartupSetup(t, server, project.ID)
			running, err := st.GetProjectByID(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if running.PID == 0 || running.Status != "starting" {
				t.Fatalf("project did not launch after setup: %+v", running)
			}
		})
	}
}

func TestStartProjectSetupFailurePreventsLaunchAndAllowsRetry(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HELPER_PROCESS", "1")
	directory := t.TempDir()
	writeStartupFiles(t, directory, map[string]string{"package.json": "{}"})
	st, project := testProject(t, app.Project{
		Name: "synthetic", Path: directory, Strategy: "package",
		Command: "echo started > runtime.started && " + daemonHelperCommand(),
	})
	server := New(st, nil)
	var attempts atomic.Int32
	server.setupRunner = projectSetupRunnerFunc(func(_ context.Context, _ app.Project, _ func(string, string) error) (projectsetup.Result, error) {
		if attempts.Add(1) == 1 {
			return projectsetup.Result{}, errors.New("synthetic install failure")
		}
		return projectsetup.Result{}, nil
	})
	t.Cleanup(func() {
		if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
			t.Errorf("stop project: %v", err)
		}
	})

	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatal(err)
	}
	waitForStartupSetup(t, server, project.ID)
	failed, err := st.GetProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "crashed" || failed.PID != 0 {
		t.Fatalf("failed install runtime = %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(directory, "runtime.started")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime launched after failed install: %v", err)
	}
	logs, err := st.Logs(context.Background(), project.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, entry := range logs {
		messages = append(messages, entry.Line)
	}
	if !strings.Contains(strings.Join(messages, "\n"), "Dependency setup failed: synthetic install failure") {
		t.Fatalf("missing install failure in logs: %v", messages)
	}
	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatal(err)
	}
	waitForStartupSetup(t, server, project.ID)
	retried, err := st.GetProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.PID == 0 || attempts.Load() != 2 {
		t.Fatalf("retry runtime = %+v, setup attempts = %d", retried, attempts.Load())
	}
}

func TestStoppingProjectCancelsDependencySetup(t *testing.T) {
	skipWindowsShellHelper(t)
	for _, action := range []string{"stop", "kill", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			directory := t.TempDir()
			writeStartupFiles(t, directory, map[string]string{"package.json": "{}"})
			st, project := testProject(t, app.Project{
				Name: "synthetic", Path: directory, Strategy: "package",
				Command: "echo started > runtime.started",
			})
			server := New(st, nil)
			started := make(chan struct{})
			server.setupRunner = projectSetupRunnerFunc(func(ctx context.Context, _ app.Project, _ func(string, string) error) (projectsetup.Result, error) {
				close(started)
				<-ctx.Done()
				return projectsetup.Result{}, ctx.Err()
			})
			t.Cleanup(func() {
				if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
					t.Errorf("stop project: %v", err)
				}
			})
			if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("dependency setup did not start")
			}
			if action == "shutdown" {
				if err := server.stopManagedApplications(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if _, err := server.stopProject(context.Background(), project.Name, action == "kill"); err != nil {
				t.Fatal(err)
			}
			stopped, err := st.GetProjectByID(context.Background(), project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stopped.Status != "stopped" || stopped.PID != 0 {
				t.Fatalf("canceled install runtime = %+v", stopped)
			}
			if _, err := os.Stat(filepath.Join(directory, "runtime.started")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime launched after cancellation: %v", err)
			}
		})
	}
}

func TestProjectSetupSurvivesDisconnectWithoutBlockingOtherProjects(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HELPER_PROCESS", "1")
	directory := t.TempDir()
	writeStartupFiles(t, directory, map[string]string{"package.json": "{}"})
	st, project := testProject(t, app.Project{
		Name: "first", Path: directory, Strategy: "package", Command: daemonHelperCommand(),
	})
	secondID, err := st.UpsertProject(context.Background(), app.Project{
		Name: "second", Path: t.TempDir(), Strategy: "custom", Command: daemonHelperCommand(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(st, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	server.setupRunner = projectSetupRunnerFunc(func(ctx context.Context, _ app.Project, _ func(string, string) error) (projectsetup.Result, error) {
		attempts.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return projectsetup.Result{}, ctx.Err()
		case <-release:
			return projectsetup.Result{}, nil
		}
	})
	t.Cleanup(func() {
		for _, name := range []string{project.Name, "second"} {
			if _, err := server.stopProject(context.Background(), name, true); err != nil {
				t.Errorf("stop project: %v", err)
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := server.startProject(ctx, project.Name, true)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("dependency setup did not start")
	}
	cancel()
	if duplicate, err := server.startProject(context.Background(), project.Name, true); err != nil || duplicate.Status != "starting" {
		t.Fatalf("duplicate Run = %+v, error = %v", duplicate, err)
	}
	projects, err := server.enriched(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 2 || projects[0].Status != "starting" {
		t.Fatalf("project list during setup = %+v", projects)
	}
	second, err := server.startProject(context.Background(), strconv.FormatInt(secondID, 10), true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Port == second.Port || second.PID == 0 {
		t.Fatalf("concurrent projects have invalid runtimes: first = %+v, second = %+v", first, second)
	}
	close(release)
	waitForStartupSetup(t, server, project.ID)
	running, err := st.GetProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.PID == 0 || attempts.Load() != 1 {
		t.Fatalf("disconnected startup = %+v, setup attempts = %d", running, attempts.Load())
	}
}

func TestStartProjectInstallsLocalNodeDevDependency(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is required for the local dependency fixture")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is required for the local dependency fixture")
	}
	t.Setenv("NODE_ENV", "production")
	t.Setenv("NPM_CONFIG_AUDIT", "false")
	t.Setenv("NPM_CONFIG_FUND", "false")
	t.Setenv("NPM_CONFIG_UPDATE_NOTIFIER", "false")
	directory := t.TempDir()
	writeStartupFiles(t, directory, map[string]string{
		"package.json":            `{"name":"synthetic-startup","version":"1.0.0","private":true,"type":"module","scripts":{"dev":"node server.mjs"},"devDependencies":{"synthetic-runtime-dependency":"file:./dependency"}}`,
		".npmrc":                  "omit=dev\n",
		"dependency/package.json": `{"name":"synthetic-runtime-dependency","version":"1.0.0","type":"module","exports":"./index.mjs"}`,
		"dependency/index.mjs":    `export const ready = "dependency-ready";`,
		"server.mjs": `import { ready } from "synthetic-runtime-dependency";
import { createServer } from "node:http";
createServer((_, response) => response.end(ready)).listen(Number(process.env.PORT), "127.0.0.1");
`,
	})
	st, project := testProject(t, app.Project{
		Name: "synthetic", Path: directory, Strategy: "package", Command: "npm run dev",
	})
	server := New(st, nil)
	server.readinessDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
			t.Errorf("stop project: %v", err)
		}
	})
	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatal(err)
	}
	waitForStartupSetupWithin(t, server, project.ID, time.Minute)
	waitForProjectStatus(t, st, project.ID, "running")
}

func TestMakeProjectUsesInstalledPythonEnvironment(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HELPER_PROCESS", "1")
	directory := t.TempDir()
	writeStartupFiles(t, directory, map[string]string{
		"Makefile":         "run:\n\tpython\n",
		"requirements.txt": "",
		"tools/make":       "#!/bin/sh\n[ \"$1\" = run ] || exit 2\ncommand -v python\nexec python\n",
	})
	makePath := filepath.Join(directory, "tools", "make")
	if err := os.Chmod(makePath, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(makePath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	st, project := testProject(t, app.Project{
		Name: "synthetic", Path: directory, Strategy: "make", Command: "make run",
	})
	server := New(st, nil)
	server.setupRunner = projectSetupRunnerFunc(func(_ context.Context, project app.Project, _ func(string, string) error) (projectsetup.Result, error) {
		bin := filepath.Join(project.Path, ".venv", "bin")
		if err := os.MkdirAll(bin, 0o700); err != nil {
			return projectsetup.Result{}, err
		}
		script := "#!/bin/sh\nprintf '%s\\n' \"$VIRTUAL_ENV\" > python.used\nexec " + daemonHelperCommand() + "\n"
		return projectsetup.Result{}, os.WriteFile(filepath.Join(bin, "python"), []byte(script), 0o700)
	})
	t.Cleanup(func() {
		if _, err := server.stopProject(context.Background(), project.Name, true); err != nil {
			t.Errorf("stop project: %v", err)
		}
	})
	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatal(err)
	}
	waitForStartupSetup(t, server, project.ID)
	deadline := time.Now().Add(3 * time.Second)
	var environment []byte
	var err error
	for time.Now().Before(deadline) {
		environment, err = os.ReadFile(filepath.Join(directory, "python.used"))
		if err == nil && len(environment) > 0 {
			break
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		runtime, runtimeErr := st.GetProjectByID(context.Background(), project.ID)
		logs, logErr := st.Logs(context.Background(), project.ID, 20)
		t.Fatalf("Make did not use the installed Python executable: %v; runtime = %+v (%v); logs = %+v (%v)", err, runtime, runtimeErr, logs, logErr)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(directory, ".venv"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(environment)); got != want {
		t.Fatalf("Python VIRTUAL_ENV = %q, want %q", got, want)
	}
}

func writeStartupFiles(t *testing.T, directory string, files map[string]string) {
	t.Helper()
	for name, contents := range files {
		path := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func waitForStartupSetup(t *testing.T, server *Server, projectID int64) {
	t.Helper()
	waitForStartupSetupWithin(t, server, projectID, 15*time.Second)
}

func waitForStartupSetupWithin(t *testing.T, server *Server, projectID int64, timeout time.Duration) {
	t.Helper()
	server.mu.Lock()
	running := server.running[projectID]
	server.mu.Unlock()
	if running == nil || running.setupDone == nil {
		return
	}
	select {
	case <-running.setupDone:
	case <-time.After(timeout):
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		logs, err := server.store.Logs(ctx, projectID, 100)
		t.Fatalf("dependency setup did not finish within %s; logs = %+v (%v)", timeout, logs, err)
	}
}
