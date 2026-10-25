package docker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"
)

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
func awaitTTY(t *testing.T, ctx context.Context, chunks <-chan string, want string) {
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
				return
			}
		case <-ctx.Done():
			t.Fatalf("native TTY never produced %q: %s", want, output.String())
		}
	}
}

func TestNativeWindowsContainersDockerTTYExecPreservesResizeAndExit(t *testing.T) {
	manager, _, _ := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	created, err := client.ExecCreate(ctx, id, dockerclient.ExecCreateOptions{TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		ConsoleSize: dockerclient.ConsoleSize{Height: 30, Width: 90}, Cmd: []string{"fixture.exe"}})
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
	_, _ = stream.Conn.Write([]byte("size\r\n"))
	awaitTTY(t, ctx, chunks, "native-tty-size:90x30")
	if _, err := client.ExecResize(ctx, created.ID, dockerclient.ExecResizeOptions{Width: 132, Height: 41}); err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Conn.Write([]byte("size\r\n"))
	awaitTTY(t, ctx, chunks, "native-tty-size:132x41")
	_, _ = stream.Conn.Write([]byte("exit\r\n"))
	awaitTTY(t, ctx, chunks, "native-tty-final-output")
	for range chunks {
	}
	state, err := client.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
	if err != nil || state.Running || state.ExitCode != 37 {
		t.Fatalf("native runhcs exit status=%+v, %v", state, err)
	}
}

func TestNativeWindowsContainersDockerTTYAttachResizesAndDisconnectDoesNotStopTask(t *testing.T) {
	manager, engine, _ := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, true)
	if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
	defer server.Close()
	client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.ContainerAttach(ctx, id, dockerclient.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	chunks := ttyChunks(stream.Reader)
	_, _ = stream.Conn.Write([]byte("size\r\n"))
	awaitTTY(t, ctx, chunks, "native-tty-size:90x30")
	if _, err := client.ContainerResize(ctx, id, dockerclient.ContainerResizeOptions{Width: 133, Height: 42}); err != nil {
		t.Fatal(err)
	}
	_, _ = stream.Conn.Write([]byte("size\r\n"))
	awaitTTY(t, ctx, chunks, "native-tty-size:133x42")
	stream.Close()
	engine.mu.Lock()
	session := engine.sessions[id]
	engine.mu.Unlock()
	status, err := session.process.Status(ctx)
	if err != nil || status.Status != "running" {
		t.Fatalf("native attach disconnect stopped task: %+v, %v", status, err)
	}
}

func TestNativeWindowsContainersTTYExecDisconnectReapsOnlyExec(t *testing.T) {
	for _, stdin := range []bool{false, true} {
		t.Run(map[bool]string{false: "output-only", true: "interactive"}[stdin], func(t *testing.T) {
			manager, engine, _ := nativeEngineFixture(t)
			id := createNativeEngineTestContainer(t, manager, false)
			if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(NewAPI(manager, manager.windows.installation.Pipe()))
			defer server.Close()
			client := dockerSDKClient(t, strings.Replace(server.URL, "http://", "tcp://", 1), dockerclient.WithAPIVersion("1.47"))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			created, err := client.ExecCreate(ctx, id, dockerclient.ExecCreateOptions{TTY: true, AttachStdin: stdin, AttachStdout: true, Cmd: []string{"fixture.exe"}})
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
				state, err := client.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if !state.Running {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("native exec disconnect did not reap process")
				case <-time.After(time.Millisecond):
				}
			}
			engine.mu.Lock()
			session := engine.sessions[id]
			engine.mu.Unlock()
			status, err := session.process.Status(ctx)
			if err != nil || status.Status != "running" {
				t.Fatal("exec disconnect killed the container init process")
			}
		})
	}
}

func TestNativeWindowsContainersTTYValidationPrecedesMutation(t *testing.T) {
	manager, _, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
		t.Fatal(err)
	}
	api := NewAPI(manager, manager.windows.installation.Pipe())
	for _, body := range []string{
		`{"Tty":true,"ConsoleSize":[32768,80],"Cmd":["fixture.exe"]}`,
		`{"Tty":true,"Privileged":true,"Cmd":["fixture.exe"]}`,
		`{"Tty":true,"WorkingDir":"/linux","Cmd":["fixture.exe"]}`,
		`{"Tty":true,"Env":["BAD\nVALUE=1"],"Cmd":["fixture.exe"]}`,
		`{"Tty":true,"DetachKeys":"ctrl-x","Cmd":["fixture.exe"]}`,
	} {
		response := httptest.NewRecorder()
		api.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/containers/"+id+"/exec", strings.NewReader(body)))
		if response.Code < 400 || len(api.execs) != 0 {
			t.Fatalf("invalid native exec mutated state: %s", response.Body.String())
		}
	}
	if store.tasks != 1 {
		t.Fatal("exec validation created an unrelated task")
	}
}
