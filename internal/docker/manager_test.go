package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type fakeRunner struct {
	mu       sync.Mutex
	commands []runtimes.Command
	outputs  map[string][]byte
	errors   map[string]error
	ordered  map[string][]runtimes.OutputChunk
	handler  func(runtimes.Command) ([]byte, error)
	streamer func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
	starter  func(runtimes.Command) (runtimes.Process, error)
}

type engineInstallRunner struct {
	mu       sync.Mutex
	created  bool
	ownerID  string
	commands []runtimes.Command
}

type concurrentInstallRunner struct {
	mu        sync.Mutex
	active    int
	maxActive int
}

type cancellationCleanupRunner struct {
	cancel  context.CancelFunc
	removed bool
}

func (r *cancellationCleanupRunner) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	args := strings.Join(command.Args, " ")
	switch args {
	case "create --name demo alpine:latest":
		return []byte("container-id\n"), nil
	case "start container-id":
		r.cancel()
		return nil, errors.New("start failed")
	case "container inspect container-id":
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cleanup context is canceled: %w", ctx.Err())
		}
		if r.removed {
			return nil, errors.New("no such container: container-id")
		}
		return []byte(`[{"Id":"container-id"}]`), nil
	case "rm --force container-id":
		if ctx.Err() != nil {
			return nil, fmt.Errorf("cleanup context is canceled: %w", ctx.Err())
		}
		r.removed = true
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected command: %s", args)
	}
}

func workingBuildKitDialer(context.Context) (net.Conn, error) {
	connection, peer := net.Pipe()
	_ = peer.Close()
	return connection, nil
}

func (f *fakeRunner) RunStreaming(
	_ context.Context,
	command runtimes.Command,
	emit func(runtimes.OutputChunk) error,
) ([]byte, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	if f.streamer != nil {
		f.mu.Unlock()
		return f.streamer(command, emit)
	}
	key := command.Name + " " + strings.Join(command.Args, " ")
	chunks := append([]runtimes.OutputChunk(nil), f.ordered[key]...)
	runErr := f.errors[key]
	f.mu.Unlock()
	var output []byte
	for _, chunk := range chunks {
		output = append(output, chunk.Data...)
		if err := emit(chunk); err != nil {
			return output, err
		}
	}
	return output, runErr
}

func (r *engineInstallRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	switch command.Name {
	case "/usr/local/bin/nerdctl":
		return []byte("containerd unavailable"), errors.New("exit 1")
	case "limactl":
		joined := strings.Join(command.Args, " ")
		switch {
		case joined == "list porto-engine --json" && !r.created:
			return nil, nil
		case joined == "list porto-engine --json" && r.created:
			return []byte(`{"name":"porto-engine","status":"Running"}` + "\n"), nil
		case strings.HasPrefix(joined, "start --tty=false"):
			r.created = true
			return nil, nil
		case joined == `shell porto-engine -- sh -c umask 077; cat > "$HOME/.porto-engine-owner"`:
			r.ownerID = strings.TrimSpace(string(command.Stdin))
			return nil, nil
		case joined == `shell porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
			return []byte(r.ownerID + "\n"), nil
		case joined == "shell porto-engine -- nerdctl version":
			return []byte("nerdctl version 2.1.0\n"), nil
		}
	}
	return nil, fmt.Errorf("unexpected command: %s %s", command.Name, strings.Join(command.Args, " "))
}

func (r *concurrentInstallRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	if command.Name != "/usr/local/bin/nerdctl" || strings.Join(command.Args, " ") != "version" {
		return nil, fmt.Errorf("unexpected command: %s %s", command.Name, strings.Join(command.Args, " "))
	}
	r.mu.Lock()
	r.active++
	r.maxActive = max(r.maxActive, r.active)
	r.mu.Unlock()
	time.Sleep(25 * time.Millisecond)
	r.mu.Lock()
	r.active--
	r.mu.Unlock()
	return []byte("nerdctl version 2.1.0\n"), nil
}

func (f *fakeRunner) Run(_ context.Context, command runtimes.Command) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	if f.handler != nil {
		return f.handler(command)
	}
	key := command.Name + " " + strings.Join(command.Args, " ")
	return f.outputs[key], f.errors[key]
}

func (f *fakeRunner) Start(_ context.Context, command runtimes.Command) (runtimes.Process, error) {
	f.mu.Lock()
	f.commands = append(f.commands, command)
	starter := f.starter
	f.mu.Unlock()
	if starter == nil {
		return nil, fmt.Errorf("unexpected streaming command: %s %s", command.Name, strings.Join(command.Args, " "))
	}
	return starter(command)
}

func TestLimaInstanceStatusTargetsEngineAndIgnoresDiagnostics(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"limactl list porto-engine --json": []byte(
				"time=\"2026-09-01T22:00:00+02:00\" level=warning msg=\"diagnostic\"\n" +
					`{"name":"porto-engine","status":"Running"}` + "\n",
			),
		},
		errors: map[string]error{},
	}
	exists, running, err := New(runner).limaInstanceStatus(context.Background())
	if err != nil {
		t.Fatalf("inspect Lima instance: %v", err)
	}
	if !exists || !running {
		t.Fatalf("status = exists:%t running:%t", exists, running)
	}
}

func TestLimaInstanceStatusTreatsUnmatchedInstanceAsMissing(t *testing.T) {
	key := "limactl list porto-engine --json"
	runner := &fakeRunner{
		outputs: map[string][]byte{
			key: []byte("level=warning msg=\"No instance matching porto-engine found.\"\nlevel=fatal msg=\"unmatched instances\"\n"),
		},
		errors: map[string]error{
			key: errors.New("exit status 1"),
		},
	}
	exists, running, err := New(runner).limaInstanceStatus(context.Background())
	if err != nil {
		t.Fatalf("inspect missing Lima instance: %v", err)
	}
	if exists || running {
		t.Fatalf("status = exists:%t running:%t", exists, running)
	}
}

func TestVerifyLimaOwnershipIgnoresLimaDiagnostics(t *testing.T) {
	const ownerID = "porto-owner"
	key := `limactl shell porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`
	runner := &fakeRunner{
		outputs: map[string][]byte{
			key: []byte(
				"time=\"2026-09-02T14:30:00+02:00\" level=warning msg=\"host agent is starting\"\n" +
					ownerID + "\n",
			),
		},
		errors: map[string]error{},
	}

	if err := New(runner).verifyLimaOwnership(context.Background(), ownerID); err != nil {
		t.Fatalf("verify ownership with Lima diagnostics: %v", err)
	}
}

func TestVerifyLimaOwnershipRejectsDifferentMarker(t *testing.T) {
	key := `limactl shell porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`
	runner := &fakeRunner{
		outputs: map[string][]byte{
			key: []byte(
				"time=\"2026-09-02T14:30:00+02:00\" level=warning msg=\"host agent is starting\"\n" +
					"porto-different-owner\n",
			),
		},
		errors: map[string]error{},
	}

	err := New(runner).verifyLimaOwnership(context.Background(), "porto-owner")
	if err == nil || !strings.Contains(err.Error(), "ownership marker does not match") {
		t.Fatalf("expected ownership mismatch, got %v", err)
	}
}

func TestManagerStatusAndInventory(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl version": []byte("nerdctl version 2.1.0\n"),
			"nerdctl ps -a --no-trunc --format {{json .}}":            []byte(`{"ID":"abc","Names":"api","Image":"porto/api","State":"running","Status":"Up","Ports":"8080/tcp","Networks":"porto","Mounts":"data","CreatedAt":"2026-08-31T12:00:00Z","Labels":"com.docker.compose.project=porto"}` + "\n"),
			"nerdctl images --digests --no-trunc --format {{json .}}": []byte(`{"ID":"sha256:1","Repository":"porto/api","Tag":"latest","Digest":"sha256:2","Size":"42MB","CreatedAt":"2026-08-31T12:00:00Z"}` + "\n"),
		},
		errors: map[string]error{},
	}

	manager := New(runner)

	status := manager.Status(context.Background(), "/tmp/porto.sock")
	if !status.Available || status.Context != "porto" || status.Endpoint != "unix:///tmp/porto.sock" {
		t.Fatalf("unexpected status: %+v", status)
	}
	containers, err := manager.Containers(context.Background())
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers) != 1 || containers[0].Name != "api" || containers[0].ComposeProject != "porto" {
		t.Fatalf("unexpected containers: %+v", containers)
	}
	images, err := manager.Images(context.Background())
	if err != nil {
		t.Fatalf("list images: %v", err)
	}
	if len(images) != 1 || images[0].Digest != "sha256:2" {
		t.Fatalf("unexpected images: %+v", images)
	}
}

func TestNormalizeNerdctlReferenceDropsTagBeforeDigest(t *testing.T) {
	got := normalizeNerdctlReference("kindest/node:v1.37.0@sha256:abcdef")
	if got != "kindest/node@sha256:abcdef" {
		t.Fatalf("normalized reference = %q", got)
	}
}

func TestContainerHostnameRejectsUnrepresentableAliases(t *testing.T) {
	_, err := containerHostname(CreateContainerRequest{
		Name: "project-api-1",
		Networks: []ContainerNetwork{{
			Name:    "project_default",
			Aliases: []string{"project-api-1", "api", "api.internal"},
		}},
	})
	if err == nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error = %v, want unsupported aliases", err)
	}
}

func TestAppendHealthcheckArgsKeepsImageCommandOverrides(t *testing.T) {
	args, err := appendHealthcheckArgs([]string{"create"}, &ContainerHealthcheck{
		Interval: 5 * time.Second,
		Timeout:  2 * time.Second,
		Retries:  4,
	})
	if err != nil {
		t.Fatalf("appendHealthcheckArgs: %v", err)
	}
	got := strings.Join(args, " ")
	want := "create --health-interval 5s --health-timeout 2s --health-retries 4"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

func TestRunContainerCleansUpWithFreshContextAfterCanceledStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancellationCleanupRunner{cancel: cancel}
	_, err := New(runner).RunContainer(ctx, CreateContainerRequest{Name: "demo", Image: "alpine:latest"})
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("RunContainer error = %v", err)
	}
	if !runner.removed {
		t.Fatal("created container was not removed after start failure")
	}
}

func TestHealthcheckDueUsesNewestResult(t *testing.T) {
	document := json.RawMessage(`{
		"Config":{"Healthcheck":{"Test":["CMD-SHELL","true"],"Interval":30000000000}},
		"State":{"Running":true,"Health":{"Log":[
			{"End":"2026-09-01T12:00:50Z"},
			{"End":"2026-09-01T12:00:00Z"}
		]}}
	}`)
	due, _, err := healthcheckDue(document, time.Date(2026, 9, 1, 12, 1, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("healthcheckDue: %v", err)
	}
	if due {
		t.Fatal("healthcheck was due before the newest result interval elapsed")
	}
}

func TestContainerActionRejectsUnsupportedAction(t *testing.T) {
	err := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}).
		ContainerAction(context.Background(), "container", "explode")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported action error, got %v", err)
	}
}

func TestContainerStartResumesPausedContainer(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect demo": []byte(`[{"State":{"Status":"paused","Paused":true}}]`),
			"nerdctl unpause demo":           nil,
		},
		errors: map[string]error{},
	}

	if err := New(runner).ContainerAction(context.Background(), "demo", "start"); err != nil {
		t.Fatalf("start paused container: %v", err)
	}
	if len(runner.commands) != 2 || strings.Join(runner.commands[1].Args, " ") != "unpause demo" {
		t.Fatalf("commands = %+v, want inspect followed by unpause", runner.commands)
	}
}

func TestContainerRemovalGuardReceivesResolvedName(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect abc123": []byte(`[{"Id":"abc123","Name":"/porto-kind-control-plane"}]`),
		},
		errors: map[string]error{},
	}
	manager := New(runner)
	manager.SetContainerRemovalGuard(func(_ context.Context, name string) error {
		if name != "porto-kind-control-plane" {
			t.Fatalf("guard name = %q", name)
		}
		return errors.New("managed Kubernetes control plane")
	})

	err := manager.ContainerAction(context.Background(), "abc123", "remove-force")
	if err == nil || !strings.Contains(err.Error(), "managed Kubernetes control plane") {
		t.Fatalf("remove-force error = %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("removal continued after guard rejection: %+v", runner.commands)
	}
}

func TestContainerTerminalCommandsSupportApplicationAndDebugShells(t *testing.T) {
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	application, err := manager.ContainerTerminalCommand(context.Background(), "demo", "sh", false)
	if err != nil {
		t.Fatalf("application terminal: %v", err)
	}
	wantApplication := []string{
		"nerdctl", "exec", "--interactive", "--tty", "demo",
		"sh", "-c", `TERM=xterm-256color COLORTERM=truecolor exec "$0" -i`, "sh",
	}
	if !reflect.DeepEqual(application.Args, wantApplication) {
		t.Fatalf("application command = %q, want %q", application.Args, wantApplication)
	}

	debug, err := manager.ContainerTerminalCommand(context.Background(), "demo", "sh", true)
	if err != nil {
		t.Fatalf("debug terminal: %v", err)
	}
	joined := strings.Join(debug.Args, " ")
	for _, expected := range []string{
		"nerdctl run --rm --interactive --tty",
		"--network container:demo",
		"--pid container:demo",
		"--volumes-from demo",
		"nicolaka/netshoot:latest /bin/bash",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("debug command missing %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "--uts") {
		t.Fatalf("debug command uses unsupported container UTS sharing: %s", joined)
	}
}

func TestForceRemoveCompletesStoppedContainerCleanup(t *testing.T) {
	cleanupAttempts := 0
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "container inspect demo":
			return []byte(`[{"Id":"original-id","Name":"/demo"}]`), nil
		case "rm --force original-id":
			return []byte("original-id\n"), nil
		case "rm original-id":
			cleanupAttempts++
			if cleanupAttempts == 1 {
				return []byte("container original-id is in running status"), errors.New("exit status 1")
			}
			return []byte("original-id\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
	}

	if err := New(runner).ContainerAction(context.Background(), "demo", "remove-force"); err != nil {
		t.Fatalf("ContainerAction: %v", err)
	}
	if cleanupAttempts != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", cleanupAttempts)
	}
}

func TestContainerRemovalCompleteRequiresContainerSpecificError(t *testing.T) {
	if !containerRemovalComplete(errors.New("no such container: demo")) {
		t.Fatal("container-specific not-found error was not accepted")
	}
	if containerRemovalComplete(errors.New("exec: nerdctl: executable file not found")) {
		t.Fatal("unrelated executable error was treated as successful removal")
	}
}

func TestForceRemoveRejectsAmbiguousContainerID(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect abc": []byte(`[{"Id":"abc-one"},{"Id":"abc-two"}]`),
		},
		errors: map[string]error{},
	}
	err := New(runner).ContainerAction(context.Background(), "abc", "remove-force")
	if err == nil || !strings.Contains(err.Error(), "matched 2 containers") {
		t.Fatalf("error = %v, want ambiguous ID rejection", err)
	}
}

func TestWaitContainerSupportsDockerNextExitCondition(t *testing.T) {
	inspects := 0
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "container inspect demo":
			inspects++
			if inspects == 1 {
				return []byte(`[{"State":{"Status":"created","Running":false,"ExitCode":0}}]`), nil
			}
			if inspects == 2 {
				return []byte(`[{"State":{"Status":"running","Running":true,"ExitCode":0,"StartedAt":"2026-09-01T16:00:00Z"}}]`), nil
			}
			return []byte(`[{"State":{"Status":"exited","Running":false,"ExitCode":0,"StartedAt":"2026-09-01T16:00:00Z","FinishedAt":"2026-09-01T16:00:01Z"}}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
	}
	code, err := New(runner).WaitContainer(context.Background(), "demo", "next-exit")
	if err != nil {
		t.Fatalf("WaitContainer: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestWaitContainerTreatsPausedTaskAsActive(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "container inspect demo":
			return []byte(`[{"State":{"Status":"paused","Running":false,"ExitCode":0}}]`), nil
		case "wait demo":
			return []byte("0\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
	}
	if _, err := New(runner).WaitContainer(context.Background(), "demo", "not-running"); err != nil {
		t.Fatalf("WaitContainer: %v", err)
	}
}

func TestWaitContainerCapturesFastNextExit(t *testing.T) {
	inspects := 0
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "container inspect demo" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		inspects++
		if inspects == 1 {
			return []byte(`[{"State":{"Status":"created","Running":false,"ExitCode":0}}]`), nil
		}
		return []byte(`[{"State":{"Status":"exited","Running":false,"ExitCode":7,"StartedAt":"2026-09-01T16:00:00Z","FinishedAt":"2026-09-01T16:00:01Z"}}]`), nil
	}

	code, err := New(runner).WaitContainer(context.Background(), "demo", "next-exit")
	if err != nil {
		t.Fatalf("WaitContainer: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
}

func TestManagerReportsMissingNativeRuntime(t *testing.T) {
	manager := NewWithStateDir(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}, t.TempDir())
	manager.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	status := manager.Status(context.Background(), "")
	if status.Available || !strings.Contains(status.Message, "engine-install") {
		t.Fatalf("unexpected unavailable status: %+v", status)
	}
}

func TestInstallDirectEnginePersistsState(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{"nerdctl version": []byte("nerdctl version 2.1.0\n")},
		errors:  map[string]error{},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.dialBuildKit = workingBuildKitDialer
	manager.lookPath = func(name string) (string, error) {
		if name == "nerdctl" {
			return "/usr/local/bin/nerdctl", nil
		}
		return "", errors.New("not found")
	}
	status, err := manager.InstallEngine(context.Background())
	if err != nil {
		t.Fatalf("install engine: %v", err)
	}
	if !status.Available {
		t.Fatalf("engine unavailable after install: %+v", status)
	}
	state, err := manager.readEngineState()
	if err != nil {
		t.Fatalf("read engine state: %v", err)
	}
	if state.Mode != "direct" {
		t.Fatalf("engine mode = %q, want direct", state.Mode)
	}
}

func TestInstallEngineSerializesConcurrentRequests(t *testing.T) {
	runner := &concurrentInstallRunner{}
	stateDir := t.TempDir()
	managers := []*Manager{
		NewWithStateDir(runner, stateDir),
		NewWithStateDir(runner, stateDir),
	}
	for _, manager := range managers {
		manager.dialBuildKit = workingBuildKitDialer
		manager.lookPath = func(name string) (string, error) {
			if name == "nerdctl" {
				return "/usr/local/bin/nerdctl", nil
			}
			return "", errors.New("not found")
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, manager := range managers {
		go func() {
			<-start
			_, err := manager.InstallEngine(context.Background())
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("install engine: %v", err)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.maxActive != 1 {
		t.Fatalf("concurrent engine installations = %d, want 1", runner.maxActive)
	}
}

func TestInstallEngineFallsBackToWritableLimaBackend(t *testing.T) {
	runner := &engineInstallRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.goos = "darwin"
	manager.dialBuildKit = workingBuildKitDialer
	manager.lookPath = func(name string) (string, error) {
		switch name {
		case "nerdctl":
			return "/usr/local/bin/nerdctl", nil
		case "limactl":
			return "/usr/local/bin/limactl", nil
		default:
			return "", errors.New("not found")
		}
	}
	status, err := manager.InstallEngine(context.Background())
	if err != nil {
		t.Fatalf("install engine: %v", err)
	}
	if !status.Available || !strings.Contains(status.Backend, "Lima") {
		t.Fatalf("unexpected engine status: %+v", status)
	}
	foundWritableMount := false
	for _, command := range runner.commands {
		if command.Name == "limactl" && strings.Contains(strings.Join(command.Args, " "), "--mount-writable") {
			foundWritableMount = true
			break
		}
	}
	if !foundWritableMount {
		t.Fatalf("Lima creation did not request writable mounts: %+v", runner.commands)
	}
}

func TestInstallEngineRejectsUnownedLimaNameCollision(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"limactl list porto-engine --json": []byte(`{"name":"porto-engine","status":"Running"}` + "\n"),
		},
		errors: map[string]error{},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.goos = "darwin"
	manager.lookPath = func(name string) (string, error) {
		if name == "limactl" {
			return "/usr/local/bin/limactl", nil
		}
		return "", errors.New("not found")
	}
	if _, err := manager.InstallEngine(context.Background()); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected ownership collision, got %v", err)
	}
}

func TestActivateAndDeactivateEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket endpoint test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-docker-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "porto.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	canonical := filepath.Join(dir, "docker.sock")
	previous := filepath.Join(dir, "previous.sock")
	if err := os.Symlink(previous, canonical); err != nil {
		t.Fatalf("create previous link: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	state, err := ActivateEndpoint(canonical, target, statePath, true)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if state.PreviousLink != previous {
		t.Fatalf("expected previous link %q, got %q", previous, state.PreviousLink)
	}
	activeTarget, err := os.Readlink(canonical)
	if err != nil || activeTarget != target {
		t.Fatalf("unexpected active link %q: %v", activeTarget, err)
	}
	status := AddEndpointStatus(Status{ProxySocket: target}, canonical, statePath)
	if !status.Canonical || status.PreviousLink != previous {
		t.Fatalf("unexpected endpoint status: %+v", status)
	}
	if err := DeactivateEndpoint(statePath); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	restored, err := os.Readlink(canonical)
	if err != nil || restored != previous {
		t.Fatalf("unexpected restored link %q: %v", restored, err)
	}
}

func TestActivateAllowsCanonicalEndpointWithoutUpstream(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket endpoint test")
	}
	dir, err := os.MkdirTemp("/tmp", "porto-native-endpoint-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	target := filepath.Join(dir, "porto.sock")
	listener, err := net.Listen("unix", target)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	canonical := filepath.Join(dir, "docker.sock")
	if _, err := ActivateEndpoint(canonical, target, filepath.Join(dir, "state.json"), false); err != nil {
		t.Fatalf("activate native engine endpoint: %v", err)
	}
}
