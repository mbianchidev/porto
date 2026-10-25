//go:build windows

package windowscontainers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	shimoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/coder/websocket"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	dockerclient "github.com/moby/moby/client"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func nativeDashboardReadUntil(t *testing.T, ctx context.Context, connection *websocket.Conn, want string) string {
	t.Helper()
	var output strings.Builder
	for !strings.Contains(output.String(), want) {
		kind, data, err := connection.Read(ctx)
		if err != nil || kind != websocket.MessageBinary {
			t.Fatalf("native dashboard terminal before %q: kind=%v data=%q, %v; output=%s", want, kind, data, err, output.String())
		}
		output.Write(data)
	}
	return output.String()
}

func nativeDashboardClose(t *testing.T, ctx context.Context, connection *websocket.Conn, code websocket.StatusCode, reason string) string {
	t.Helper()
	var output strings.Builder
	for {
		kind, data, err := connection.Read(ctx)
		if err != nil {
			var closed websocket.CloseError
			if !errors.As(err, &closed) || closed.Code != code || closed.Reason != reason {
				t.Fatalf("native dashboard terminal close = %v, want %d %q; output=%s", err, code, reason, output.String())
			}
			return output.String()
		}
		if kind != websocket.MessageBinary {
			t.Fatalf("native dashboard output used a nonbinary frame: %v", kind)
		}
		output.Write(data)
	}
}

func runNativeDashboardWebSocket(t *testing.T, parent context.Context, installation Installation, docker *dockerclient.Client, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	connection, err := grpc.NewClient("passthrough:///porto-native-dashboard-acceptance",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeContext(ctx, installation.ContainerdPipe())
		}))
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := containerd.NewWithConn(connection, containerd.WithDefaultNamespace(Namespace))
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	defer sdk.Close()
	nativeContext := namespaces.WithNamespace(ctx, Namespace)
	container, err := sdk.LoadContainer(nativeContext, id)
	if err != nil {
		t.Fatal(err)
	}
	task, err := container.Task(nativeContext, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a separately owned exec alive to prove that disconnect only reaps
	// the dashboard exec, not container init or another interactive session.
	unrelated, err := docker.ExecCreate(ctx, id, dockerclient.ExecCreateOptions{
		TTY: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Cmd: []string{`C:\fixture\native-fixture.exe`, "tty"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unrelatedStream, err := docker.ExecAttach(ctx, unrelated.ID, dockerclient.ExecAttachOptions{TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer unrelatedStream.Close()
	unrelatedChunks := nativeTTYChunks(unrelatedStream.Reader)
	nativeTTYExpect(t, ctx, unrelatedChunks, "porto-native-tty-ready")
	unrelatedState, err := docker.ExecInspect(ctx, unrelated.ID, dockerclient.ExecInspectOptions{})
	if err != nil || !unrelatedState.Running || unrelatedState.PID == 0 {
		t.Fatalf("unrelated native exec is not live: %+v, %v", unrelatedState, err)
	}
	pids := func() []uint32 {
		t.Helper()
		processes, err := task.Pids(nativeContext)
		if err != nil {
			t.Fatalf("read real native container processes through SDK: %v", err)
		}
		result := make([]uint32, 0, len(processes))
		for _, process := range processes {
			result = append(result, process.Pid)
		}
		slices.Sort(result)
		return result
	}
	baseline := pids()
	if !slices.Contains(baseline, task.Pid()) || !slices.Contains(baseline, uint32(unrelatedState.PID)) {
		t.Fatalf("native process enumeration omitted init or unrelated exec: init=%d exec=%d pids=%v", task.Pid(), unrelatedState.PID, baseline)
	}
	var dashboardExecs []string
	trackDashboardExec := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			processes, err := task.Pids(nativeContext)
			if err != nil {
				t.Fatal(err)
			}
			for _, process := range processes {
				if slices.Contains(baseline, process.Pid) {
					continue
				}
				var details shimoptions.ProcessDetails
				if process.Info == nil || process.Info.UnmarshalTo(&details) != nil {
					t.Fatalf("native process %d omitted maintained runhcs process details", process.Pid)
				}
				if details.ExecID == "" {
					continue
				}
				exec, err := task.LoadProcess(nativeContext, details.ExecID, nil)
				if err != nil {
					t.Fatalf("load real dashboard SDK exec %s: %v", details.ExecID, err)
				}
				status, err := exec.Status(nativeContext)
				// LoadProcess does not populate its client-side Pid field.
				// The authoritative PID is the task's live HCS enumeration.
				if err != nil || status.Status != containerd.Running || process.Pid == 0 ||
					process.Pid == task.Pid() || process.Pid == uint32(unrelatedState.PID) {
					t.Fatalf("dashboard SDK exec is not a real live process: PID=%d status=%+v, %v", process.Pid, status, err)
				}
				dashboardExecs = append(dashboardExecs, details.ExecID)
			}
			if len(dashboardExecs) == 1 {
				return
			}
			if len(dashboardExecs) > 1 || time.Now().After(deadline) {
				t.Fatalf("dashboard terminal must own exactly one new SDK exec: %v", dashboardExecs)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	assertOnlyDashboardReaped := func() {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			actual := pids()
			reaped := slices.Equal(actual, baseline)
			for _, id := range dashboardExecs {
				_, err := task.LoadProcess(nativeContext, id, nil)
				if err == nil {
					reaped = false
				} else if !errdefs.IsNotFound(err) {
					t.Fatalf("verify deleted dashboard SDK exec %s: %v", id, err)
				}
			}
			if reaped {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("dashboard exec was not reaped, or another process was killed: before=%v after=%v", baseline, actual)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		state, err := task.Status(nativeContext)
		unrelatedAfter, inspectErr := docker.ExecInspect(ctx, unrelated.ID, dockerclient.ExecInspectOptions{})
		if err != nil || state.Status != containerd.Running || inspectErr != nil || !unrelatedAfter.Running ||
			unrelatedAfter.PID != unrelatedState.PID {
			t.Fatalf("dashboard exec cleanup changed init or unrelated task: init=%+v %v unrelated=%+v %v", state, err, unrelatedAfter, inspectErr)
		}
		dashboardExecs = nil
	}
	client := ClientFor(installation)
	defer client.CloseIdleConnections()
	address := fmt.Sprintf("ws://localhost/api/docker/containers/%s/terminal?shell=cmd.exe", id)
	dial := func() *websocket.Conn {
		t.Helper()
		terminal, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: client})
		if err != nil {
			t.Fatalf("open actual native worker dashboard WebSocket: %v", err)
		}
		t.Cleanup(func() { _ = terminal.CloseNow() })
		return terminal
	}
	denied, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: client,
		HTTPHeader: http.Header{"Origin": {"https://foreign.example.test"}}})
	if denied != nil {
		denied.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("native dashboard terminal bypassed loopback origin policy: response=%v, %v", response, err)
	}
	if response.Body != nil {
		response.Body.Close()
	}
	assertOnlyDashboardReaped()
	terminal := dial()
	trackDashboardExec()
	write := func(kind websocket.MessageType, text string) {
		t.Helper()
		if err := terminal.Write(ctx, kind, []byte(text)); err != nil {
			t.Fatalf("write actual dashboard terminal: %v", err)
		}
	}
	write(websocket.MessageBinary, "C:\\fixture\\native-fixture.exe tty\r\n")
	nativeDashboardReadUntil(t, ctx, terminal, "porto-native-tty-ready")
	write(websocket.MessageBinary, "wait-size 120 32\r\n")
	nativeDashboardReadUntil(t, ctx, terminal, "porto-native-tty-size:120x32")
	write(websocket.MessageText, `{"type":"resize","cols":132,"rows":41}`)
	write(websocket.MessageBinary, "wait-size 132 41\r\n")
	nativeDashboardReadUntil(t, ctx, terminal, "porto-native-tty-size:132x41")
	write(websocket.MessageBinary, "quit\r\n")
	nativeDashboardReadUntil(t, ctx, terminal, "porto-native-tty-final-output")
	write(websocket.MessageBinary, "echo porto-native-dashboard-final-output\r\nexit 37\r\n")
	if output := nativeDashboardClose(t, ctx, terminal, websocket.StatusInternalError, "terminal exited with exit code 37"); !strings.Contains(output, "porto-native-dashboard-final-output") {
		t.Fatalf("native dashboard discarded final output before exit 37: %s", output)
	}
	assertOnlyDashboardReaped()
	terminal = dial()
	trackDashboardExec()
	write(websocket.MessageBinary, "echo porto-native-dashboard-success\r\nexit 0\r\n")
	if output := nativeDashboardClose(t, ctx, terminal, websocket.StatusNormalClosure, "terminal session complete"); !strings.Contains(output, "porto-native-dashboard-success") {
		t.Fatalf("native dashboard discarded successful terminal output: %s", output)
	}
	assertOnlyDashboardReaped()

	for _, interruption := range []string{"disconnect", "client cancellation", "invalid resize"} {
		terminal = dial()
		trackDashboardExec()
		write(websocket.MessageBinary, "echo porto-native-dashboard-live\r\n")
		nativeDashboardReadUntil(t, ctx, terminal, "porto-native-dashboard-live")
		live := pids()
		if len(live) <= len(baseline) {
			t.Fatalf("dashboard terminal did not create a real additional native exec: before=%v after=%v", baseline, live)
		}
		for _, pid := range baseline {
			if !slices.Contains(live, pid) {
				t.Fatalf("opening dashboard terminal changed an unrelated process: before=%v after=%v", baseline, live)
			}
		}
		switch interruption {
		case "disconnect":
			_ = terminal.CloseNow()
		case "client cancellation":
			readContext, stopRead := context.WithCancel(ctx)
			stopRead()
			var err error
			for err == nil {
				_, _, err = terminal.Read(readContext)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("client read cancellation did not close its WebSocket: %v", err)
			}
		case "invalid resize":
			write(websocket.MessageText, `{"type":"resize","cols":0,"rows":41}`)
			nativeDashboardClose(t, ctx, terminal, websocket.StatusInternalError, "terminal session ended")
		}
		assertOnlyDashboardReaped()
		t.Logf("actual native dashboard WebSocket %s reaped only its exec", interruption)
	}
	if _, err := unrelatedStream.Conn.Write([]byte("quit\r\n")); err != nil {
		t.Fatal(err)
	}
	nativeTTYExpect(t, ctx, unrelatedChunks, "porto-native-tty-final-output")
	nativeTTYFinish(t, ctx, unrelatedChunks)
	unrelatedAfter, err := docker.ExecInspect(ctx, unrelated.ID, dockerclient.ExecInspectOptions{})
	if err != nil || unrelatedAfter.Running || unrelatedAfter.ExitCode != 37 {
		t.Fatalf("unrelated native exec lost its own exit status: %+v, %v", unrelatedAfter, err)
	}
	t.Log("current native worker dashboard WebSocket preserved binary output, initial dimensions, resize, exit 37, cancellation and exec-only reaping")
}
