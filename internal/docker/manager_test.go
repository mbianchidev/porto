package docker

import (
	"bytes"
	"context"
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
	mu               sync.Mutex
	created          bool
	ownerID          string
	binfmtConfigured bool
	binfmtErr        error
	removed          bool
	commands         []runtimes.Command
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

func workingBuildKitDialer(t *testing.T) func(context.Context) (net.Conn, error) {
	t.Helper()
	return buildKitControlTestDialer(t, &buildKitPlatformServer{ready: true})
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
		case joined == `shell --workdir=/ porto-engine -- sh -c umask 077; cat > "$HOME/.porto-engine-owner"`:
			r.ownerID = strings.TrimSpace(string(command.Stdin))
			return nil, nil
		case joined == `shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`:
			return []byte(r.ownerID + "\n"), nil
		case strings.HasPrefix(joined, "shell --workdir=/ porto-engine -- sudo -n sh -c "):
			if r.ownerID == "" {
				return nil, errors.New("binfmt setup preceded ownership verification")
			}
			r.binfmtConfigured = true
			return []byte("configure guest QEMU/binfmt\n"), r.binfmtErr
		case joined == "shell --workdir=/ porto-engine -- nerdctl version":
			return []byte("nerdctl version 2.1.0\n"), nil
		case joined == "delete --force porto-engine":
			r.removed = true
			return nil, nil
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

func TestAuthenticatedPullUsesTemporaryConfigInsideLima(t *testing.T) {
	config := []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"dGVzdDp0b2tlbg=="}}}`)
	runner := &fakeRunner{
		outputs: map[string][]byte{},
		errors:  map[string]error{},
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name != "limactl" {
			return nil, fmt.Errorf("unexpected command: %s", command.Name)
		}
		if len(command.Args) < 9 ||
			!reflect.DeepEqual(command.Args[:4], []string{"shell", "porto-engine", "--", "sh"}) ||
			command.Args[4] != "-c" ||
			command.Args[6] != "porto-registry-auth" ||
			!reflect.DeepEqual(command.Args[7:], []string{"nerdctl", "pull", "docker.io/kindest/node:v1.36.0"}) {
			return nil, fmt.Errorf("unexpected Lima pull arguments: %v", command.Args)
		}
		script := command.Args[5]
		if !strings.Contains(script, "umask 077") ||
			!strings.Contains(script, `trap 'rm -rf "$config_dir"' EXIT`) ||
			!strings.Contains(script, `DOCKER_CONFIG="$config_dir" "$@"`) {
			return nil, fmt.Errorf("Lima pull does not securely scope credentials: %s", script)
		}
		if !bytes.Equal(command.Stdin, config) {
			return nil, errors.New("Lima pull did not receive the scoped Docker config")
		}
		return nil, nil
	}
	manager := New(runner)

	_, err := manager.runBackendWithDockerConfig(
		context.Background(),
		commandBackend{name: "limactl", limaInstance: "porto-engine"},
		time.Minute,
		"pull Porto image",
		config,
		"pull",
		"docker.io/kindest/node:v1.36.0",
	)
	if err != nil {
		t.Fatalf("authenticated Lima pull: %v", err)
	}
}

func TestPullImageUsesConfiguredRegistryAuthResolver(t *testing.T) {
	var configData []byte
	runner := &fakeRunner{
		outputs: map[string][]byte{},
		errors:  map[string]error{},
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if command.Name != "nerdctl" ||
			!reflect.DeepEqual(command.Args, []string{"pull", "ghcr.io/example/private:latest"}) {
			return nil, fmt.Errorf("unexpected command: %s %v", command.Name, command.Args)
		}
		for _, entry := range command.Env {
			if configDir, ok := strings.CutPrefix(entry, "DOCKER_CONFIG="); ok {
				var err error
				configData, err = os.ReadFile(filepath.Join(configDir, "config.json"))
				return nil, err
			}
		}
		return nil, errors.New("authenticated pull did not set DOCKER_CONFIG")
	}
	manager := New(runner)
	manager.SetRegistryAuthResolver(func(_ context.Context, reference string) (*RegistryAuth, error) {
		if reference != "ghcr.io/example/private:latest" {
			return nil, fmt.Errorf("unexpected reference %q", reference)
		}
		return &RegistryAuth{
			Username:      "octocat",
			Password:      "test-token",
			ServerAddress: "ghcr.io",
		}, nil
	})

	if err := manager.PullImage(context.Background(), "ghcr.io/example/private:latest", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(configData), `"ghcr.io"`) ||
		!strings.Contains(string(configData), `"auth":"b2N0b2NhdDp0ZXN0LXRva2Vu"`) {
		t.Fatalf("scoped registry config = %s", configData)
	}
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
	key := `limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`
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
	key := `limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`
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

func TestEngineGuestProbesDoNotEnterHostMounts(t *testing.T) {
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "limactl" || len(command.Args) < 3 ||
			!reflect.DeepEqual(command.Args[:3], []string{"shell", "--workdir=/", engineInstanceName}) {
			return nil, fmt.Errorf("probe depends on the mounted host working directory: %v", command.Args)
		}
		switch command.Args[len(command.Args)-1] {
		case `cat "$HOME/.porto-engine-owner"`:
			return []byte("test-owner\n"), nil
		case limaContainerdDiscoveryCommand:
			return []byte("/run/user/1000/containerd/containerd.sock\ndefault\n"), nil
		default:
			return nil, nil
		}
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	t.Run("ownership write", func(t *testing.T) {
		if err := manager.writeLimaOwnership(context.Background(), "test-owner"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ownership read", func(t *testing.T) {
		if err := manager.verifyLimaOwnership(context.Background(), "test-owner"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("socket discovery", func(t *testing.T) {
		socket, namespace, err := manager.discoverLimaContainerd(context.Background(), engineInstanceName)
		if err != nil {
			t.Fatal(err)
		}
		if socket != "/run/user/1000/containerd/containerd.sock" || namespace != "default" {
			t.Fatalf("socket discovery = %q, %q", socket, namespace)
		}
	})
	t.Run("runtime helper", func(t *testing.T) {
		client := grpcContainerRuntime{lima: engineInstanceName, runner: runner}
		if _, err := client.runRuntimeHelper(context.Background(), "probe"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEngineTimeoutIncludesCommandDiagnostics(t *testing.T) {
	t.Setenv("LIMA_HOME", t.TempDir())
	const diagnostic = "ssh: connect to host 127.0.0.1: connection timed out"
	runner := &fakeRunner{
		handler: func(runtimes.Command) ([]byte, error) {
			return []byte(diagnostic), context.DeadlineExceeded
		},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := New(runner).verifyLimaOwnership(ctx, "test-owner")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), diagnostic) ||
		!strings.Contains(err.Error(), "verify Porto engine ownership") {
		t.Fatalf("timeout discarded the guest diagnostic: %v", err)
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

func TestContainerNameResolvesInspectIdentity(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"nerdctl container inspect abc123": []byte(`[{"Id":"abc123","Name":"/porto-kind-control-plane"}]`),
		},
		errors: map[string]error{},
	}
	name, err := New(runner).ContainerName(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("resolve container name: %v", err)
	}
	if name != "porto-kind-control-plane" {
		t.Fatalf("container name = %q", name)
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
	if strings.Contains(joined, "--uts") || strings.Contains(joined, "--ipc") {
		t.Fatalf("debug command uses target namespace modes that are not generally shareable: %s", joined)
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

func TestWaitContainerSupportsDockerRemovedCondition(t *testing.T) {
	inspects := 0
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "container inspect demo" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		inspects++
		if inspects == 1 {
			return []byte(`[{"State":{"Status":"exited","Running":false,"ExitCode":17}}]`), nil
		}
		return nil, errors.New("no such container: demo")
	}

	code, err := New(runner).WaitContainer(context.Background(), "demo", "removed")
	if err != nil {
		t.Fatalf("WaitContainer: %v", err)
	}
	if code != 17 {
		t.Fatalf("exit code = %d, want 17", code)
	}
}

func TestWaitContainerRemovedRejectsUnknownContainer(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "container inspect missing" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		return nil, errors.New("no such container: missing")
	}

	if _, err := New(runner).WaitContainer(context.Background(), "missing", "removed"); err == nil {
		t.Fatal("unknown container removal wait succeeded")
	}
}

func TestContainerStartObservedRejectsRemovalWithoutStartEvidence(t *testing.T) {
	runner := &fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		if strings.Join(command.Args, " ") != "container inspect demo" {
			return nil, fmt.Errorf("unexpected command: %+v", command)
		}
		return nil, errors.New("no such container: demo")
	}
	observed, err := New(runner).containerStartObserved(
		context.Background(),
		"demo",
		containerStartBaseline{},
	)
	if err != nil {
		t.Fatalf("containerStartObserved: %v", err)
	}
	if observed {
		t.Fatal("container removal without task-start evidence was accepted")
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
	manager.dialBuildKit = workingBuildKitDialer(t)
	manager.inventory = newContainerInventory(nil, defaultInventoryOptions())
	_, manager.inventoryCancel = context.WithCancel(context.Background())
	defer manager.inventoryCancel()
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
	for _, command := range runner.commands {
		if command.Name == "limactl" {
			t.Fatalf("direct backend tried to provision guest emulation: %+v", command)
		}
	}
	select {
	case <-manager.inventory.refresh:
	default:
		t.Fatal("successful engine installation did not wake the container inventory")
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
		manager.dialBuildKit = workingBuildKitDialer(t)
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

func TestLimaEngineTemplate(t *testing.T) {
	for _, test := range []struct {
		goos string
		want string
	}{
		{goos: "windows", want: "template://ubuntu-24.04"},
		{goos: "darwin", want: "template://default"},
		{goos: "linux", want: "template://default"},
	} {
		t.Run(test.goos, func(t *testing.T) {
			if got := limaEngineTemplate(test.goos); got != test.want {
				t.Fatalf("engine template = %q, want %q", got, test.want)
			}
		})
	}
}

func TestInstallEngineFallsBackToWritableLimaBackend(t *testing.T) {
	runner := &engineInstallRunner{}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.dialBuildKit = workingBuildKitDialer(t)
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
			template := "template://default"
			if runtime.GOOS == "windows" {
				template = "template://ubuntu-24.04"
			}
			if got := command.Args[len(command.Args)-1]; got != template {
				t.Fatalf("created engine from %q, want %q", got, template)
			}
			foundWritableMount = true
			break
		}
	}
	if !foundWritableMount {
		t.Fatalf("Lima creation did not request writable mounts: %+v", runner.commands)
	}
	if !runner.binfmtConfigured {
		t.Fatal("Lima installation did not configure multi-platform execution")
	}
}

func TestInstallEngineReconcilesMultiPlatformBuilds(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, setupFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing=%t/failure=%t", existing, setupFails), func(t *testing.T) {
				runner := &engineInstallRunner{created: existing, ownerID: "test-owner"}
				if setupFails {
					runner.binfmtErr = errors.New("QEMU package installation failed")
				}
				manager := NewWithStateDir(runner, t.TempDir())
				manager.dialBuildKit = workingBuildKitDialer(t)
				manager.lookPath = func(name string) (string, error) {
					if name == "limactl" {
						return name, nil
					}
					return "", errors.New("not found")
				}
				if existing {
					if err := manager.writeEngineState(engineState{
						Mode: "lima", Instance: engineInstanceName, OwnerID: runner.ownerID,
					}); err != nil {
						t.Fatal(err)
					}
				}

				_, err := manager.InstallEngine(context.Background())
				if setupFails {
					if err == nil || !strings.Contains(err.Error(), runner.binfmtErr.Error()) {
						t.Fatalf("provisioning error = %v, want QEMU installation failure", err)
					}
					if runner.removed != !existing {
						t.Fatalf("removed engine = %t, existing = %t", runner.removed, existing)
					}
					state, stateErr := manager.readEngineState()
					if existing && (stateErr != nil || state.OwnerID != "test-owner") {
						t.Fatalf("failed upgrade changed engine ownership: %+v, %v", state, stateErr)
					}
					if !existing && !errors.Is(stateErr, os.ErrNotExist) {
						t.Fatalf("failed installation persisted state: %v", stateErr)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if existing {
					for _, command := range runner.commands {
						if command.Name == "limactl" && command.Args[0] == "start" {
							t.Fatalf("reconciliation recreated the existing running engine: %+v", command)
						}
					}
				}
				if !runner.binfmtConfigured {
					t.Fatal("engine installation skipped multi-platform setup")
				}
			})
		}
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

func TestStartEngineRefreshesInventory(t *testing.T) {
	for _, phase := range []string{"Stopped", "Running"} {
		t.Run(phase, func(t *testing.T) {
			runner := &fakeRunner{
				outputs: map[string][]byte{
					"limactl list porto-engine --json":                                                []byte(`{"name":"porto-engine","status":"` + phase + `"}`),
					`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("test-owner\n"),
				},
			}
			manager := NewWithStateDir(runner, t.TempDir())
			manager.dialBuildKit = workingBuildKitDialer(t)
			if err := manager.writeEngineState(engineState{
				Mode: "lima", Instance: engineInstanceName, OwnerID: "test-owner",
			}); err != nil {
				t.Fatal(err)
			}
			manager.inventory = newContainerInventory(nil, defaultInventoryOptions())
			_, manager.inventoryCancel = context.WithCancel(context.Background())
			defer manager.inventoryCancel()
			if err := manager.StartEngine(context.Background()); err != nil {
				t.Fatal(err)
			}
			configured := false
			for _, command := range runner.commands {
				if strings.HasPrefix(strings.Join(command.Args, " "), "shell --workdir=/ porto-engine -- sudo -n sh -c ") {
					configured = true
				}
			}
			if !configured {
				t.Fatal("engine startup skipped multi-platform setup")
			}
			select {
			case <-manager.inventory.refresh:
			default:
				t.Fatal("starting the engine did not wake the container inventory")
			}
		})
	}
}

func TestEngineProvisioningRejectsForeignOwnership(t *testing.T) {
	for _, action := range []string{"install", "start"} {
		t.Run(action, func(t *testing.T) {
			runner := &fakeRunner{
				outputs: map[string][]byte{
					"limactl list porto-engine --json":                                                []byte(`{"name":"porto-engine","status":"Running"}`),
					`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("another-owner\n"),
				},
			}
			manager := NewWithStateDir(runner, t.TempDir())
			manager.lookPath = func(name string) (string, error) { return name, nil }
			if err := manager.writeEngineState(engineState{
				Mode: "lima", Instance: engineInstanceName, OwnerID: "test-owner",
			}); err != nil {
				t.Fatal(err)
			}
			var err error
			if action == "install" {
				_, err = manager.InstallEngine(context.Background())
			} else {
				err = manager.StartEngine(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("foreign engine ownership was accepted: %v", err)
			}
			for _, command := range runner.commands {
				if strings.Contains(strings.Join(command.Args, " "), "sudo") {
					t.Fatalf("foreign engine was modified: %+v", command)
				}
			}
		})
	}
}

func TestStartEngineReportsBinfmtFailure(t *testing.T) {
	runner := &fakeRunner{
		outputs: map[string][]byte{
			"limactl list porto-engine --json":                                                []byte(`{"name":"porto-engine","status":"Running"}`),
			`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("test-owner\n"),
		},
		errors: map[string]error{
			"limactl shell --workdir=/ porto-engine -- sudo -n sh -c " + limaBinfmtInstallCommand: errors.New("binfmt registration failed"),
		},
	}
	manager := NewWithStateDir(runner, t.TempDir())
	if err := manager.writeEngineState(engineState{
		Mode: "lima", Instance: engineInstanceName, OwnerID: "test-owner",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.StartEngine(context.Background()); err == nil || !strings.Contains(err.Error(), "binfmt registration failed") {
		t.Fatalf("startup suppressed provisioning failure: %v", err)
	}
	for _, command := range runner.commands {
		if command.Args[0] == "stop" || command.Args[0] == "delete" {
			t.Fatalf("failed provisioning destroyed existing engine state: %+v", command)
		}
	}
}

func TestResolveRuntimeHelperPathPrefersPackagedHelper(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "porto.exe")
	helper := filepath.Join(root, "runtime", "bin", "porto-runtime-helper")
	if err := os.MkdirAll(filepath.Dir(helper), 0o755); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := os.WriteFile(helper, []byte("helper"), 0o644); err != nil {
		t.Fatalf("write runtime helper: %v", err)
	}

	path, err := resolveRuntimeHelperPath(executable, func(string) (string, error) {
		return "", errors.New("PATH lookup should not run")
	})
	if err != nil {
		t.Fatalf("resolve runtime helper: %v", err)
	}
	if path != helper {
		t.Fatalf("runtime helper = %q, want %q", path, helper)
	}
}

func TestInstallLimaRuntimeHelperPreservesPreviousBinaryOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runtime helper installation runs in the Linux guest")
	}
	home := t.TempDir()
	directory := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(directory, "porto-runtime-helper")
	previous := []byte("previous helper")
	if err := os.WriteFile(installed, previous, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "invalid-helper")
	if err := os.WriteFile(replacement, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
		if command.Name != "limactl" || len(command.Args) < 6 {
			return nil, fmt.Errorf("unexpected helper installation: %+v", command)
		}
		return (runtimes.ExecRunner{}).Run(ctx, runtimes.Command{
			Name:  "sh",
			Args:  []string{"-c", command.Args[len(command.Args)-1]},
			Env:   []string{"HOME=" + home},
			Stdin: command.Stdin,
		})
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) {
		if name == "porto-runtime-helper" {
			return replacement, nil
		}
		return "", errors.New("not found")
	}

	if err := manager.installLimaRuntimeHelper(ctx, "test-engine"); err == nil {
		t.Fatal("invalid helper was accepted")
	}
	current, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, previous) {
		t.Fatalf("failed helper installation replaced the working binary: %q", current)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "porto-runtime-helper" {
		t.Fatalf("failed helper installation left temporary files: %+v", entries)
	}
}

func TestResolveRuntimeHelperPathFallsBackToPath(t *testing.T) {
	expected := filepath.Join(t.TempDir(), "porto-runtime-helper")
	path, err := resolveRuntimeHelperPath(filepath.Join(t.TempDir(), "porto"), func(name string) (string, error) {
		if name != "porto-runtime-helper" {
			t.Fatalf("lookup name = %q", name)
		}
		return expected, nil
	})
	if err != nil {
		t.Fatalf("resolve runtime helper: %v", err)
	}
	if path != expected {
		t.Fatalf("runtime helper = %q, want %q", path, expected)
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
