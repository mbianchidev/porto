package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/gitutil"
	"github.com/mbianchidev/porto/internal/killswitch"
	"github.com/mbianchidev/porto/internal/process"
	projectsetup "github.com/mbianchidev/porto/internal/setup"
	"github.com/mbianchidev/porto/internal/store"
)

type killSwitchRunner struct {
	mu       sync.Mutex
	syncArgs chan []string
}

func (r *killSwitchRunner) LookPath(name string) (string, error) {
	if name == "killswitchctl" {
		return "/Users/test/bin/killswitchctl", nil
	}
	return "", errors.New("not found")
}

func (r *killSwitchRunner) Run(_ context.Context, _ string, args ...string) (killswitch.CommandOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case len(args) > 1 && args[1] == "sync-ports":
		r.syncArgs <- append([]string(nil), args...)
		return killswitch.CommandOutput{
			Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"userPorts":[3000],"integrationPorts":{"porto":[41001]},"effectivePorts":[3000,41001]}`),
		}, nil
	case len(args) > 1 && args[1] == "cleanup":
		return killswitch.CommandOutput{
			Stdout: []byte(`{"version":"1.4.0","autoKillEnabled":true,"candidateCount":1,"killedCount":1,"killedProcesses":[{"pid":123,"command":"node vite","runtime":"node","ageHours":14}]}`),
		}, nil
	default:
		return killswitch.CommandOutput{}, errors.New("unexpected command")
	}
}

type noopKillSwitchInstaller struct{}

func (noopKillSwitchInstaller) Install(context.Context) error { return nil }

type fakeSetupRunner struct {
	started  chan struct{}
	release  chan struct{}
	parallel bool
}

func (f fakeSetupRunner) Run(_ context.Context, _ app.Project, emit func(stream, line string) error) (projectsetup.Result, error) {
	if f.started != nil {
		close(f.started)
	}
	if f.release != nil {
		<-f.release
	}
	if f.parallel {
		errs := make(chan error, 2)
		go func() { errs <- emit("stdout", "dependencies installed") }()
		go func() { errs <- emit("stderr", "install warning") }()
		return projectsetup.Result{Commands: []string{"npm ci"}}, errors.Join(<-errs, <-errs)
	}
	if err := emit("stdout", "dependencies installed"); err != nil {
		return projectsetup.Result{}, err
	}
	return projectsetup.Result{Commands: []string{"npm ci"}}, nil
}

type failingSetupRunner struct {
	cancel context.CancelFunc
}

func (f failingSetupRunner) Run(_ context.Context, project app.Project, _ func(stream, line string) error) (projectsetup.Result, error) {
	if err := os.WriteFile(filepath.Join(project.Path, "setup.partial"), []byte("partial"), 0o600); err != nil {
		return projectsetup.Result{}, err
	}
	if f.cancel != nil {
		f.cancel()
	}
	return projectsetup.Result{Commands: []string{"npm ci"}}, errors.New("dependency setup failed")
}

func TestAvailableBranchHostnameCompactsAndDisambiguates(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:         "2dnd",
		Path:         t.TempDir(),
		SourcePath:   t.TempDir(),
		Strategy:     "package",
		Command:      "npm run dev",
		Hostname:     "2dnd",
		BaseHostname: "2dnd",
	})
	server := New(st, nil)
	got, err := server.availableBranchHostname(context.Background(), project, "copilot/improve-elemental-resistances-system", "main")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2dnd-cop-imp-ele-res-sys" {
		t.Fatalf("hostname = %q", got)
	}
	if _, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "collision",
		Path:     t.TempDir(),
		Strategy: "go",
		Command:  "go run .",
		Hostname: got,
	}); err != nil {
		t.Fatal(err)
	}
	disambiguated, err := server.availableBranchHostname(context.Background(), project, "copilot/improve-elemental-resistances-system", "main")
	if err != nil {
		t.Fatal(err)
	}
	if disambiguated == got || !strings.HasPrefix(disambiguated, got+"-") {
		t.Fatalf("disambiguated hostname = %q", disambiguated)
	}
}

func TestProjectSetupSerializesParallelLogs(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	server := New(st, nil)
	server.setupRunner = fakeSetupRunner{parallel: true}
	mux := http.NewServeMux()
	server.routes(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/web/setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("setup status = %d: %s", response.Code, response.Body.String())
	}
	logs, err := st.Logs(context.Background(), project.ID, 20)
	if err != nil {
		t.Fatalf("load setup logs: %v", err)
	}
	if len(logs) != 4 {
		t.Fatalf("setup logs = %#v, want four lines", logs)
	}
}

func TestProjectSetupWritesLogs(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	server := New(st, nil)
	server.setupRunner = fakeSetupRunner{}
	mux := http.NewServeMux()
	server.routes(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/web/setup", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("setup status = %d: %s", response.Code, response.Body.String())
	}
	logs, err := st.Logs(context.Background(), project.ID, 20)
	if err != nil {
		t.Fatalf("load setup logs: %v", err)
	}
	got := make([]string, 0, len(logs))
	for _, line := range logs {
		got = append(got, line.Line)
	}
	want := []string{"Dependency setup started.", "dependencies installed", "Dependency setup completed."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("logs = %#v, want %#v", got, want)
	}
}

func TestProjectSetupRejectsConcurrentRequest(t *testing.T) {
	st, _ := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	started := make(chan struct{})
	release := make(chan struct{})
	server := New(st, nil)
	server.setupRunner = fakeSetupRunner{started: started, release: release}
	mux := http.NewServeMux()
	server.routes(mux)

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/projects/web/setup", nil))
	}()
	<-started

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/web/setup", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("concurrent setup status = %d, want %d", response.Code, http.StatusConflict)
	}
	close(release)
	<-firstDone
}

func TestProjectStaysStartingUntilHTTPReadinessPasses(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_PROCESS", "1")
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_DELAY", "150ms")
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_STATUS", strconv.Itoa(http.StatusOK))
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "custom",
		Command:  daemonHTTPHelperCommand(),
	})
	server := New(st, nil)
	server.readinessDelay = 10 * time.Millisecond

	started, err := server.startProject(context.Background(), project.Name, true)
	if err != nil {
		t.Fatalf("start project: %v", err)
	}
	if started.Status != "starting" {
		t.Fatalf("initial status = %q, want starting", started.Status)
	}
	waitForProjectStatus(t, st, project.ID, "running")

	if _, err := server.stopProject(context.Background(), project.Name, false); err != nil {
		t.Fatalf("stop project: %v", err)
	}
}

func TestProjectRemainsStartingForNonReadyHTTPResponse(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_PROCESS", "1")
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_STATUS", strconv.Itoa(http.StatusServiceUnavailable))
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "custom",
		Command:  daemonHTTPHelperCommand(),
	})
	server := New(st, nil)
	server.readinessDelay = 10 * time.Millisecond

	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatalf("start project: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	got, err := st.GetProjectByID(context.Background(), project.ID)
	if err != nil {
		t.Fatalf("load project: %v", err)
	}
	if got.Status != "starting" {
		t.Fatalf("status = %q, want starting", got.Status)
	}

	if _, err := server.stopProject(context.Background(), project.Name, false); err != nil {
		t.Fatalf("stop project: %v", err)
	}
}

func TestProjectCrashWinsBeforeHTTPReadiness(t *testing.T) {
	skipWindowsShellHelper(t)
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_PROCESS", "1")
	t.Setenv("PORTO_DAEMON_HTTP_HELPER_EXIT", "1")
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "custom",
		Command:  daemonHTTPHelperCommand(),
	})
	server := New(st, nil)
	server.readinessDelay = 10 * time.Millisecond

	if _, err := server.startProject(context.Background(), project.Name, true); err != nil {
		t.Fatalf("start project: %v", err)
	}
	waitForProjectStatus(t, st, project.ID, "crashed")
}

func TestCreateInstanceRollsBackFailedSetupAfterRequestCancellation(t *testing.T) {
	repo := initDaemonTestRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"scripts":{"dev":"vite"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runDaemonTestGit(t, repo, "add", "package.json")
	runDaemonTestGit(t, repo, "commit", "-m", "add package")
	runDaemonTestGit(t, repo, "branch", "feature")
	t.Setenv("PORTO_HOME", t.TempDir())

	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     repo,
		Strategy: "package",
		Command:  "npm run dev",
	})
	server := New(st, nil)
	requestContext, cancel := context.WithCancel(context.Background())
	server.setupRunner = failingSetupRunner{cancel: cancel}
	mux := http.NewServeMux()
	server.routes(mux)

	request := httptest.NewRequest(http.MethodPost, "/api/projects/"+strconv.FormatInt(project.ID, 10)+"/instances", strings.NewReader(`{"branch":"feature"}`))
	request = request.WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("projects after rollback = %+v", projects)
	}
	if err := gitutil.CanCheckout(repo, "feature"); err != nil {
		t.Fatalf("feature branch still has a worktree: %v", err)
	}
}

func TestDeleteManagedInstanceDiscardsDirtyWorktree(t *testing.T) {
	repo := initDaemonTestRepo(t)
	runDaemonTestGit(t, repo, "branch", "feature")
	resolved, err := gitutil.ResolveBranch(repo, "feature")
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := gitutil.CreateWorktree(repo, t.TempDir(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, source := testProject(t, app.Project{
		Name:     "web",
		Path:     repo,
		Strategy: "package",
		Command:  "npm run dev",
	})
	instanceID, err := st.UpsertProject(context.Background(), app.Project{
		Name:            source.Name,
		Path:            worktree,
		SourcePath:      repo,
		Strategy:        source.Strategy,
		Command:         source.Command,
		ManagedInstance: true,
		Branch:          "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := New(st, nil)
	mux := http.NewServeMux()
	server.routes(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/projects/"+strconv.FormatInt(instanceID, 10)+"/instance", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if _, err := st.GetProjectByID(context.Background(), instanceID); !IsNotFound(err) {
		t.Fatalf("deleted instance lookup error = %v", err)
	}

	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/projects/"+strconv.FormatInt(source.ID, 10)+"/instance", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("source delete status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestDeletingInstanceBlocksNewLifecycleOperations(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:            "web",
		Path:            t.TempDir(),
		SourcePath:      t.TempDir(),
		Strategy:        "package",
		Command:         "npm run dev",
		ManagedInstance: true,
	})
	server := New(st, nil)
	server.deleting[project.ID] = true

	if _, err := server.startProject(context.Background(), strconv.FormatInt(project.ID, 10), true); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("start error = %v", err)
	}
	if _, err := server.runProjectSetup(context.Background(), project); !errors.Is(err, errProjectSetupConflict) {
		t.Fatalf("setup error = %v", err)
	}
}

type fakeComposeCleanup struct {
	mu       sync.Mutex
	projects []app.Project
	err      error
}

func (f *fakeComposeCleanup) Down(_ context.Context, project app.Project) error {
	f.mu.Lock()
	f.projects = append(f.projects, project)
	f.mu.Unlock()
	return f.err
}

func (f *fakeComposeCleanup) calls() []app.Project {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]app.Project(nil), f.projects...)
}

func TestKillComposeProjectCleansContainersWithoutTrackedProcess(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  "docker compose -f compose.yaml up",
	})
	if err := st.SetRuntime(context.Background(), project.ID, "stopped", 0, 41001); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	cleanup := &fakeComposeCleanup{}
	server := New(st, nil)
	server.compose = cleanup

	got, err := server.stopProject(context.Background(), project.Name, true)
	if err != nil {
		t.Fatalf("kill project: %v", err)
	}
	if got.Status != "stopped" || got.PID != 0 {
		t.Fatalf("project = %+v", got)
	}
	calls := cleanup.calls()
	if len(calls) != 1 || calls[0].ID != project.ID || calls[0].Port != 41001 {
		t.Fatalf("cleanup calls = %+v", calls)
	}
}

func TestKillComposeProjectReapsTrackedLauncher(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  "docker compose -f compose.yaml up",
	})
	if err := st.SetRuntime(context.Background(), project.ID, "running", 1, 41002); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	project, err := st.GetProject(context.Background(), project.Name)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}

	cleanup := &fakeComposeCleanup{}
	server := New(st, nil)
	server.compose = cleanup
	startTestProjectProcess(t, server, project)

	got, err := server.stopProject(context.Background(), project.Name, true)
	if err != nil {
		t.Fatalf("kill project: %v", err)
	}
	if got.Status != "stopped" || got.PID != 0 {
		t.Fatalf("project = %+v", got)
	}
	server.mu.Lock()
	_, stillRunning := server.running[project.ID]
	server.mu.Unlock()
	if stillRunning {
		t.Fatal("tracked launcher was not reaped")
	}
	if calls := cleanup.calls(); len(calls) != 1 || calls[0].Command != project.Command {
		t.Fatalf("cleanup calls = %+v", calls)
	}
}

func TestKillComposeProjectStopsLauncherWhenCleanupFails(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  "docker compose -f compose.yaml up",
	})
	if err := st.SetRuntime(context.Background(), project.ID, "running", 1, 41004); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	project, err := st.GetProject(context.Background(), project.Name)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	server := New(st, nil)
	server.compose = &fakeComposeCleanup{err: errors.New("docker daemon unavailable")}
	startTestProjectProcess(t, server, project)

	_, err = server.stopProject(context.Background(), project.Name, true)
	if err == nil || !strings.Contains(err.Error(), "docker daemon unavailable") {
		t.Fatalf("error = %v", err)
	}
	got, err := st.GetProject(context.Background(), project.Name)
	if err != nil {
		t.Fatalf("reload stopped project: %v", err)
	}
	if got.Status != "stopped" || got.PID != 0 {
		t.Fatalf("project = %+v", got)
	}
	server.mu.Lock()
	_, stillRunning := server.running[project.ID]
	server.mu.Unlock()
	if stillRunning {
		t.Fatal("tracked launcher was not force-stopped after cleanup failure")
	}
}

func TestKillComposeProjectReportsCleanupFailure(t *testing.T) {
	st, project := testProject(t, app.Project{
		Name:     "web",
		Path:     t.TempDir(),
		Strategy: "compose",
		Command:  "docker compose -f compose.yaml up",
	})
	if err := st.SetRuntime(context.Background(), project.ID, "running", 123, 41003); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	server := New(st, nil)
	server.compose = &fakeComposeCleanup{err: errors.New("docker daemon unavailable")}

	_, err := server.stopProject(context.Background(), project.Name, true)
	if err == nil || !strings.Contains(err.Error(), "docker daemon unavailable") {
		t.Fatalf("error = %v", err)
	}
	got, err := st.GetProject(context.Background(), project.Name)
	if err != nil {
		t.Fatalf("reload project: %v", err)
	}
	if got.Status != "running" || got.PID != 123 {
		t.Fatalf("failed cleanup changed runtime: %+v", got)
	}
}

func TestComposeCleanupOnlyRunsForForceKill(t *testing.T) {
	for _, test := range []struct {
		name     string
		strategy string
		force    bool
	}{
		{name: "normal compose stop", strategy: "compose", force: false},
		{name: "non-compose kill", strategy: "package", force: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, project := testProject(t, app.Project{
				Name:     "web",
				Path:     t.TempDir(),
				Strategy: test.strategy,
				Command:  "npm run dev",
			})
			cleanup := &fakeComposeCleanup{}
			server := New(st, nil)
			server.compose = cleanup

			if _, err := server.stopProject(context.Background(), project.Name, test.force); err != nil {
				t.Fatalf("stop project: %v", err)
			}
			if calls := cleanup.calls(); len(calls) != 0 {
				t.Fatalf("cleanup calls = %+v", calls)
			}
		})
	}
}

func TestDaemonHelperProcess(t *testing.T) {
	if os.Getenv("PORTO_DAEMON_HELPER_PROCESS") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestDaemonHTTPHelperProcess(t *testing.T) {
	if os.Getenv("PORTO_DAEMON_HTTP_HELPER_PROCESS") != "1" {
		return
	}
	if os.Getenv("PORTO_DAEMON_HTTP_HELPER_EXIT") == "1" {
		os.Exit(12)
	}
	if delay := os.Getenv("PORTO_DAEMON_HTTP_HELPER_DELAY"); delay != "" {
		parsed, err := time.ParseDuration(delay)
		if err != nil {
			os.Exit(13)
		}
		time.Sleep(parsed)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", os.Getenv("PORT")))
	if err != nil {
		os.Exit(14)
	}
	status, err := strconv.Atoi(os.Getenv("PORTO_DAEMON_HTTP_HELPER_STATUS"))
	if err != nil {
		os.Exit(15)
	}
	_ = http.Serve(listener, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	os.Exit(0)
}

func daemonHTTPHelperCommand() string {
	executable := os.Args[0]
	return fmt.Sprintf("%q -test.run=TestDaemonHTTPHelperProcess", executable)
}

func skipWindowsShellHelper(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-backed helper process is Unix-specific")
	}
}

func waitForProjectStatus(t *testing.T, st *store.Store, projectID int64, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		project, err := st.GetProjectByID(context.Background(), projectID)
		if err != nil {
			t.Fatalf("load project: %v", err)
		}
		if project.Status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	project, err := st.GetProjectByID(context.Background(), projectID)
	if err != nil {
		t.Fatalf("load project after timeout: %v", err)
	}
	t.Fatalf("status = %q, want %q", project.Status, want)
}

func testProject(t *testing.T, project app.Project) (*store.Store, app.Project) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	id, err := st.UpsertProject(context.Background(), project)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	project.ID = id
	return st, project
}

func startTestProjectProcess(t *testing.T, server *Server, project app.Project) *projectProcess {
	t.Helper()
	cmd, stdout, stderr, err := process.Command(
		context.Background(),
		t.TempDir(),
		os.Args[0],
		"-test.run=^TestDaemonHelperProcess$",
	)
	if err != nil {
		t.Fatalf("create helper command: %v", err)
	}
	cmd.Env = append(os.Environ(), "PORTO_DAEMON_HELPER_PROCESS=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper command: %v", err)
	}
	running := &projectProcess{
		cmd:     cmd,
		done:    make(chan struct{}),
		project: project,
	}
	server.mu.Lock()
	server.running[project.ID] = running
	server.mu.Unlock()
	go server.waitForProject(project, running, project.Port)
	t.Cleanup(func() {
		_ = process.Kill(cmd)
		select {
		case <-running.done:
		case <-time.After(time.Second):
		}
		_ = stdout.Close()
		_ = stderr.Close()
	})
	return running
}

func TestKillSwitchRoutesSyncActivePortsAndRunCleanup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KillSwitch integration is supported only on macOS")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	projectPath := t.TempDir()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "web",
		Path:     projectPath,
		Strategy: "package.json",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.SetRuntime(context.Background(), id, "running", 123, 41001); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	settings.KillSwitchEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	runner := &killSwitchRunner{syncArgs: make(chan []string, 1)}
	server := New(st, nil)
	server.killSwitch = killswitch.NewManager(runner, noopKillSwitchInstaller{})
	server.running[id] = &projectProcess{
		cmd:     &exec.Cmd{},
		project: app.Project{Status: "running"},
	}
	mux := http.NewServeMux()
	server.routes(mux)

	syncResponse := httptest.NewRecorder()
	mux.ServeHTTP(syncResponse, httptest.NewRequest(http.MethodPost, "/api/integrations/kill-switch/sync", nil))
	if syncResponse.Code != http.StatusAccepted {
		t.Fatalf("sync status = %d, body = %s", syncResponse.Code, syncResponse.Body.String())
	}
	select {
	case args := <-runner.syncArgs:
		want := []string{"dev-cleanup", "sync-ports", "--source", "porto", "--ports", "41001", "--json"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("sync args = %q, want %q", args, want)
		}
	case <-time.After(time.Second):
		t.Fatal("sync command did not run")
	}
	waitForKillSwitchReady(t, server.killSwitch)

	cleanupResponse := httptest.NewRecorder()
	mux.ServeHTTP(cleanupResponse, httptest.NewRequest(http.MethodPost, "/api/integrations/kill-switch/cleanup", nil))
	if cleanupResponse.Code != http.StatusOK {
		t.Fatalf("cleanup status = %d, body = %s", cleanupResponse.Code, cleanupResponse.Body.String())
	}
	var result killswitch.CleanupResult
	if err := json.NewDecoder(cleanupResponse.Body).Decode(&result); err != nil {
		t.Fatalf("decode cleanup: %v", err)
	}
	if result.CandidateCount != 1 || result.KilledCount != 1 || len(result.KilledProcesses) != 1 || result.KilledProcesses[0].PID != 123 {
		t.Fatalf("cleanup result = %+v", result)
	}
}

func waitForKillSwitchReady(t *testing.T, manager *killswitch.Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if manager.Snapshot().State == "ready" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("KillSwitch status = %+v", manager.Snapshot())
}

func TestLogRoutesFilterAndClearStreams(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.AddLog(context.Background(), id, "stdout", "ready"); err != nil {
		t.Fatalf("add stdout: %v", err)
	}
	if err := st.AddLog(context.Background(), id, "stderr", "warning"); err != nil {
		t.Fatalf("add stderr: %v", err)
	}

	mux := http.NewServeMux()
	New(st, nil).routes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/projects/app/logs?stream=stdout", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get logs status = %d: %s", response.Code, response.Body.String())
	}
	var lines []app.LogLine
	if err := json.NewDecoder(response.Body).Decode(&lines); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Stream != "stdout" || lines[0].Line != "ready" {
		t.Fatalf("stdout logs = %+v", lines)
	}

	response = httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/projects/app/logs/clear?stream=stderr", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("clear logs status = %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Deleted int64 `json:"deleted"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("decode clear response: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", result.Deleted)
	}
}

func TestProxyUsesConfiguredHostname(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxied"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	_, rawPort, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("parse backend address: %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "application",
		Hostname: "custom-app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.SetRuntime(context.Background(), id, "running", 123, port); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	server := New(st, nil)
	trackProxyProject(server, id, port)

	request := httptest.NewRequest(http.MethodGet, "https://custom-app.porto.local/", nil)
	request.Host = "custom-app.porto.local:37681"
	response := httptest.NewRecorder()
	server.proxyByHost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
}

func TestProxyUsesDottedProjectHostname(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("dotted"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	_, rawPort, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("parse backend address: %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("parse backend port: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	id, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "devoidofbeauty.com",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := st.SetRuntime(context.Background(), id, "running", 123, port); err != nil {
		t.Fatalf("set runtime: %v", err)
	}
	server := New(st, nil)
	trackProxyProject(server, id, port)

	request := httptest.NewRequest(http.MethodGet, "https://devoidofbeauty.com.porto.localhost/", nil)
	response := httptest.NewRecorder()
	server.proxyByHost(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "dotted" {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
}

func TestProxyRejectsUntrackedStalePort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("different app"))
	}))
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, rawPort, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	st, project := testProject(t, app.Project{
		Name:     "application",
		Hostname: "stale-app",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	})
	if err := st.SetRuntime(context.Background(), project.ID, "crashed", 0, port); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "https://stale-app.porto.local/", nil)
	response := httptest.NewRecorder()
	New(st, nil).proxyByHost(response, request)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "different app") {
		t.Fatalf("proxy response = %d %q", response.Code, response.Body.String())
	}
}

func trackProxyProject(server *Server, projectID int64, port int) {
	server.running[projectID] = &projectProcess{
		cmd: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}},
		project: app.Project{
			ID:     projectID,
			Port:   port,
			Status: "starting",
		},
	}
}

func initDaemonTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runDaemonTestGit(t, repo, "init", "-b", "main")
	runDaemonTestGit(t, repo, "config", "user.email", "test@example.com")
	runDaemonTestGit(t, repo, "config", "user.name", "Test User")
	runDaemonTestGit(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDaemonTestGit(t, repo, "add", "README.md")
	runDaemonTestGit(t, repo, "commit", "-m", "initial")
	return repo
}

func runDaemonTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestProjectCertificateHostnamesIncludeBothDomains(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if _, err := st.UpsertProject(context.Background(), app.Project{
		Name:     "devoidofbeauty.com",
		Path:     t.TempDir(),
		Strategy: "package",
		Command:  "npm run dev",
	}); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	got, err := New(st, nil).projectCertificateHostnames(context.Background())
	if err != nil {
		t.Fatalf("project certificate hostnames: %v", err)
	}
	want := []string{
		"devoidofbeauty.com.porto.local",
		"devoidofbeauty.com.porto.localhost",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostnames = %v, want %v", got, want)
	}
}

func TestLocalHostnameSupportsTLSAndLegacyDomains(t *testing.T) {
	for host, want := range map[string]string{
		"app.porto.local:37681":              "app",
		"devoidofbeauty.com.porto.local":     "devoidofbeauty.com",
		"devoidofbeauty.com.porto.localhost": "devoidofbeauty.com",
		"app.porto.localhost:37680":          "app",
		"PORTO.LOCAL.":                       "",
	} {
		got, ok := localHostname(host)
		if !ok || got != want {
			t.Fatalf("localHostname(%q) = %q, %t; want %q, true", host, got, ok, want)
		}
	}
	if _, ok := localHostname("example.com"); ok {
		t.Fatal("non-Porto hostname was accepted")
	}
}

func TestDiscoverCopilotWorktrees(t *testing.T) {
	home := t.TempDir()
	worktree := filepath.Join(home, ".copilot", "copilot-worktrees", "porto", "devoidofbeauty.com")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module example.com/app\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "porto.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	server := New(st, nil)
	server.userHomeDir = func() (string, error) { return home, nil }

	count, err := server.discoverCopilotWorktrees(context.Background())
	if err != nil {
		t.Fatalf("discover worktrees: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	projects, err := st.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	canonicalWorktree, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		t.Fatalf("canonicalize worktree: %v", err)
	}
	if len(projects) != 1 || projects[0].Path != canonicalWorktree {
		t.Fatalf("projects = %+v, want discovered worktree", projects)
	}
}
