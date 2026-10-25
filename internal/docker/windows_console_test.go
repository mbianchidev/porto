package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
	dockerclient "github.com/moby/moby/client"
	"golang.org/x/term"
)

func TestNativeWindowsContainersTTYChild(t *testing.T) {
	mode := os.Getenv("PORTO_TEST_NATIVE_TTY_CHILD")
	if mode == "" {
		return
	}
	fmt.Println("native-tty-ready")
	if mode == "quiet" {
		for {
			time.Sleep(time.Second)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch strings.TrimSpace(scanner.Text()) {
		case "size":
			w, h, err := term.GetSize(int(os.Stdout.Fd()))
			fmt.Printf("native-tty-size:%dx%d:%v\n", w, h, err)
		case "exit":
			fmt.Println("native-tty-final-output")
			os.Exit(1)
		}
	}
}

type nativeTTYRunner struct {
	*fakeRunner
	muPTY     sync.Mutex
	processes []runtimes.Process
	startPTY  func(context.Context, runtimes.Command, [2]uint) (runtimes.Process, error)
}

func (r *nativeTTYRunner) StartPTY(ctx context.Context, command runtimes.Command, size [2]uint) (runtimes.Process, error) {
	process, err := r.startPTY(ctx, command, size)
	if err == nil {
		r.muPTY.Lock()
		r.processes = append(r.processes, process)
		r.muPTY.Unlock()
	}
	return process, err
}

func nativeTTYManager(t *testing.T, mode string) (*Manager, *nativeTTYRunner) {
	t.Helper()
	runner := &nativeTTYRunner{fakeRunner: &fakeRunner{}}
	manager := nativeTestManager(t, runner)
	if err := os.MkdirAll(manager.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if len(args) >= 2 && args[0] == "container" && args[1] == "inspect" {
			return []byte(`[{"Id":"native-container","State":{"Running":true,"Status":"running"},"Config":{"Tty":true,"Image":"synthetic","Labels":{"io.porto.container.console-size":"[30,90]"}}}]`), nil
		}
		if len(args) >= 2 && args[0] == "image" && args[1] == "inspect" {
			return []byte(`[{"Id":"synthetic","Os":"windows","Architecture":"amd64","OsVersion":"10.0.26100.1"}]`), nil
		}
		t.Fatalf("unexpected native TTY command: %q", args)
		return nil, nil
	}
	runner.startPTY = func(ctx context.Context, command runtimes.Command, size [2]uint) (runtimes.Process, error) {
		args := nativeCommandArguments(t, manager, command)
		if len(args) > 2 && args[0] == "--log-file" {
			if !strings.HasPrefix(args[1], manager.stateDir+string(os.PathSeparator)) {
				t.Fatalf("exec diagnostic escaped owned store: %q", args)
			}
			if err := os.WriteFile(args[1], []byte("time=\"synthetic\" level=fatal msg=\"exec failed with exit code 37\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args = args[2:]
		}
		if len(args) < 2 || (args[0] != "exec" && args[0] != "attach" && args[0] != "start") {
			t.Fatalf("unsupported native terminal command: %q", args)
		}
		return (runtimes.ExecRunner{}).StartPTY(ctx, runtimes.Command{Name: os.Args[0], Args: []string{"-test.run=^TestNativeWindowsContainersTTYChild$"},
			Env: []string{"PORTO_TEST_NATIVE_TTY_CHILD=" + mode}}, size)
	}
	t.Cleanup(func() {
		runner.muPTY.Lock()
		processes := append([]runtimes.Process(nil), runner.processes...)
		runner.muPTY.Unlock()
		for _, process := range processes {
			_ = process.Kill()
			_ = process.Wait()
		}
	})
	return manager, runner
}

func ttyChunks(reader io.Reader) <-chan string {
	chunks := make(chan string, 64)
	go func() {
		defer close(chunks)
		data := make([]byte, 4096)
		for {
			n, err := reader.Read(data)
			if n > 0 {
				chunks <- string(data[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return chunks
}
func awaitTTY(t *testing.T, ctx context.Context, chunks <-chan string, want string) string {
	t.Helper()
	var output strings.Builder
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("native TTY closed before %q: %s", want, output.String())
			}
			output.WriteString(chunk)
			if strings.Contains(output.String(), want) {
				return output.String()
			}
		case <-ctx.Done():
			t.Fatalf("native TTY did not produce %q: %s", want, output.String())
		}
	}
}

func TestNativeWindowsContainersDockerTTYExecPreservesResizeAndExit(t *testing.T) {
	manager, _ := nativeTTYManager(t, "echo")
	api := NewAPI(manager, manager.windows.installation.Pipe())
	server := httptest.NewServer(api)
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := client.ExecCreate(ctx, "native-container", dockerclient.ExecCreateOptions{TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		ConsoleSize: dockerclient.ConsoleSize{Height: 30, Width: 90}, Cmd: []string{"synthetic.exe"}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := client.ExecAttach(ctx, created.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	chunks := ttyChunks(attached.Reader)
	awaitTTY(t, ctx, chunks, "native-tty-ready")
	if _, err := attached.Conn.Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, chunks, "native-tty-size:90x30")
	if _, err := client.ExecResize(ctx, created.ID, dockerclient.ExecResizeOptions{Width: 132, Height: 41}); err != nil {
		t.Fatal(err)
	}
	if _, err := attached.Conn.Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, chunks, "native-tty-size:132x41")
	if _, err := attached.Conn.Write([]byte("exit\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, chunks, "native-tty-final-output")
	for range chunks {
	}
	status, err := client.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
	if err != nil || status.Running || status.ExitCode != 37 {
		t.Fatalf("native TTY exec exit=%+v, %v", status, err)
	}
	if files, _ := filepath.Glob(filepath.Join(manager.stateDir, "native-exec-*.log")); len(files) != 0 {
		t.Fatalf("private native exec diagnostics leaked: %v", files)
	}
}

func TestNativeWindowsContainersDockerTTYDisconnectReapsQuietCommand(t *testing.T) {
	manager, runner := nativeTTYManager(t, "quiet")
	server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := client.ExecCreate(ctx, "native-container", dockerclient.ExecCreateOptions{TTY: true, AttachStdin: true, AttachStdout: true, Cmd: []string{"synthetic.exe"}})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := client.ExecAttach(ctx, created.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, ttyChunks(attached.Reader), "native-tty-ready")
	attached.Close()
	for {
		status, err := client.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("disconnected quiet native exec was not reaped")
		case <-time.After(10 * time.Millisecond):
		}
	}
	runner.muPTY.Lock()
	process := runner.processes[0]
	runner.muPTY.Unlock()
	if err := process.Wait(); err == nil {
		t.Fatal("quiet disconnected native command reported a natural success")
	}
}

func TestNativeWindowsContainersTTYWithoutInputStillReapsOnDisconnect(t *testing.T) {
	manager, _ := nativeTTYManager(t, "quiet")
	server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := client.ExecCreate(ctx, "native-container", dockerclient.ExecCreateOptions{TTY: true, AttachStdout: true, Cmd: []string{"synthetic.exe"}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.ExecAttach(ctx, created.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, ttyChunks(stream.Reader), "native-tty-ready")
	stream.Close()
	for {
		result, err := client.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Running {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("native output-only TTY leaked after disconnect")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestNativeWindowsContainersTTYWorkerCancellationClosesAndReaps(t *testing.T) {
	manager, _ := nativeTTYManager(t, "quiet")
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	api := NewAPI(manager, manager.windows.installation.Pipe())
	api.SetLifecycleContext(lifetime)
	server := httptest.NewServer(api)
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := client.ExecCreate(ctx, "native-container", dockerclient.ExecCreateOptions{TTY: true, AttachStdin: true, AttachStdout: true, Cmd: []string{"synthetic.exe"}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.ExecAttach(ctx, created.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	chunks := ttyChunks(stream.Reader)
	awaitTTY(t, ctx, chunks, "native-tty-ready")
	stop()
	for {
		select {
		case _, ok := <-chunks:
			if !ok {
				return
			}
		case <-ctx.Done():
			t.Fatal("worker cancellation did not close the native TTY stream")
		}
	}
}

func TestNativeWindowsContainersTTYValidationPrecedesExecRegistration(t *testing.T) {
	manager, runner := nativeTTYManager(t, "echo")
	api := NewAPI(manager, manager.windows.installation.Pipe())
	for _, body := range []string{
		`{"Tty":true,"ConsoleSize":[32768,80],"Cmd":["synthetic.exe"]}`,
		`{"Tty":true,"Privileged":true,"Cmd":["synthetic.exe"]}`,
		`{"Tty":true,"WorkingDir":"/linux","Cmd":["synthetic.exe"]}`,
		`{"Tty":true,"Env":["BAD\nVALUE=1"],"Cmd":["synthetic.exe"]}`,
		`{"Tty":true,"DetachKeys":"ctrl-x","Cmd":["synthetic.exe"]}`,
	} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/native-container/exec", strings.NewReader(body)))
		if response.Code < 400 {
			t.Fatalf("invalid native TTY request registered: %s", response.Body.String())
		}
		if len(api.execs) != 0 || len(runner.processes) != 0 {
			t.Fatal("invalid native TTY request mutated exec state")
		}
	}
}

type nativeTTYConsole struct {
	mu         sync.Mutex
	configured [2]uint
	resized    [2]uint32
}

func (*nativeTTYConsole) Namespace() string                             { return "porto-windows" }
func (*nativeTTYConsole) Backend() string                               { return "native-test" }
func (*nativeTTYConsole) Snapshot(context.Context) ([]Container, error) { return nil, nil }
func (*nativeTTYConsole) Subscribe(context.Context) (<-chan ContainerLifecycleEvent, <-chan error) {
	return nil, nil
}
func (*nativeTTYConsole) Close() error { return nil }
func (r *nativeTTYConsole) ConfigureConsole(_ context.Context, _ string, size [2]uint) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configured = size
	return nil
}
func (r *nativeTTYConsole) ResizeConsole(_ context.Context, _ string, w, h uint32) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resized = [2]uint32{w, h}
	return nil
}

func TestNativeWindowsContainersDockerTTYAttachResizesAndReapsOnlyItsClient(t *testing.T) {
	manager, runner := nativeTTYManager(t, "echo")
	console := &nativeTTYConsole{}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
		return windowsInventory{containerRuntime: console}, nil
	}
	server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attached, err := client.ContainerAttach(ctx, "native-container", dockerclient.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	chunks := ttyChunks(attached.Reader)
	awaitTTY(t, ctx, chunks, "native-tty-ready")
	if _, err := client.ContainerResize(ctx, "native-container", dockerclient.ContainerResizeOptions{Width: 133, Height: 42}); err != nil {
		t.Fatal(err)
	}
	console.mu.Lock()
	resized := console.resized
	console.mu.Unlock()
	if resized != [2]uint32{133, 42} {
		t.Fatalf("native task resize did not receive requested size: %v", resized)
	}
	if _, err := attached.Conn.Write([]byte("size\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitTTY(t, ctx, chunks, "native-tty-size:133x42")
	attached.Close()
	for range chunks {
	}
	runner.muPTY.Lock()
	process := runner.processes[0]
	runner.muPTY.Unlock()
	reaped := make(chan error, 1)
	go func() { reaped <- process.Wait() }()
	select {
	case <-reaped:
	case <-ctx.Done():
		t.Fatal("native attach client was not reaped after disconnect")
	}
	deadline := time.Now().Add(time.Second)
	for {
		manager.windows.attachMu.Lock()
		count := len(manager.windows.attachProcesses)
		manager.windows.attachMu.Unlock()
		if count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native attach console registry leaked")
		}
		time.Sleep(time.Millisecond)
	}
	for _, command := range runner.commands {
		args := nativeCommandArguments(t, manager, command)
		if args[0] == "stop" || args[0] == "kill" || args[0] == "rm" {
			t.Fatalf("TTY disconnect changed a container instead of only its client: %q", args)
		}
	}
}

func TestNativeWindowsContainersTTYCreateUsesConPTYAndInitialConsoleMetadata(t *testing.T) {
	manager, runner := nativeTTYManager(t, "echo")
	console := &nativeTTYConsole{}
	manager.runtimeConnector = func(context.Context) (containerRuntime, error) {
		return windowsInventory{containerRuntime: console}, nil
	}
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	runner.startPTY = func(ctx context.Context, command runtimes.Command, size [2]uint) (runtimes.Process, error) {
		args := nativeCommandArguments(t, manager, command)
		if args[0] != "create" || !strings.Contains(strings.Join(args, " "), "--tty") {
			t.Fatalf("native TTY create escaped ConPTY: %q", args)
		}
		fake := newFakeProcess(func([]byte) ([]byte, []byte, error) { return []byte("\x1b[?25h" + id + "\r\n\x1b[?25l"), nil, nil })
		return &windowsAttachProcess{Process: fake, release: func() {}}, nil
	}
	result, err := manager.CreateContainer(context.Background(), CreateContainerRequest{Image: "synthetic", TTY: true, Interactive: true, ConsoleSize: [2]uint{30, 90}})
	if err != nil || result != id {
		t.Fatalf("native TTY create=%s, %v", result, err)
	}
	if console.configured != [2]uint{30, 90} {
		t.Fatalf("native initial console configuration=%v", console.configured)
	}
}
func TestNativeWindowsContainersExecExitCodeOnlyUsesPrivateCLILog(t *testing.T) {
	for _, data := range []string{
		`time="synthetic" level=fatal msg="exec failed with exit code 37"`,
		`{"level":"fatal","msg":"exec failed with exit code 37"}`,
	} {
		if code, ok := windowsExecLogExitCode([]byte(data)); !ok || code != 37 {
			t.Fatalf("CLI exit parser=%d,%v", code, ok)
		}
	}
	for _, data := range []string{`exec failed with exit code 37`, `{"level":"info","msg":"exec failed with exit code 37"}`, `{"level":"fatal","msg":"exec failed with exit code -1"}`} {
		if _, ok := windowsExecLogExitCode([]byte(data)); ok {
			t.Fatalf("untrusted/nonfatal diagnostic accepted: %s", data)
		}
	}
}
