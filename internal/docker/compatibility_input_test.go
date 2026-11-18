//go:build !windows

package docker

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/mbianchidev/porto/internal/runtimes"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type taskInputTasksClient struct {
	tasksapi.TasksClient
	mu      sync.Mutex
	gets    []*tasktypes.Process
	closeIO []*tasksapi.CloseIORequest
}

func (f *taskInputTasksClient) Get(_ context.Context, _ *tasksapi.GetRequest, _ ...grpc.CallOption) (*tasksapi.GetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.gets) == 0 || f.gets[0] == nil {
		if len(f.gets) > 0 {
			f.gets = f.gets[1:]
		}
		return nil, status.Error(codes.NotFound, "task not found")
	}
	process := f.gets[0]
	if len(f.gets) > 1 {
		f.gets = f.gets[1:]
	}
	return &tasksapi.GetResponse{Process: process}, nil
}

func (f *taskInputTasksClient) CloseIO(_ context.Context, request *tasksapi.CloseIORequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeIO = append(f.closeIO, request)
	return &emptypb.Empty{}, nil
}

func TestForwardTaskInputFeedsNewTaskAndClosesItsInput(t *testing.T) {
	directory := t.TempDir()
	fifo := filepath.Join(directory, "demo-stdin")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	go func() {
		file, err := os.Open(fifo)
		if err != nil {
			received <- "open: " + err.Error()
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		received <- string(data)
	}()
	tasks := &taskInputTasksClient{gets: []*tasktypes.Process{
		nil,
		{Status: tasktypes.Status_STOPPED, Stdin: filepath.Join(directory, "previous-stdin")},
		{Status: tasktypes.Status_CREATED, Stdin: fifo},
	}}
	previousInterval := compatibilityInputPollInterval
	compatibilityInputPollInterval = time.Millisecond
	t.Cleanup(func() { compatibilityInputPollInterval = previousInterval })
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	runtime := &grpcContainerRuntime{tasks: tasks, namespace: "default", runner: runtimes.ExecRunner{}}

	if err := runtime.ForwardTaskInput(context.Background(), "demo", strings.NewReader("piped input\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "piped input\n" {
			t.Fatalf("container stdin = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("container stdin did not reach EOF")
	}
	if len(tasks.closeIO) != 1 || tasks.closeIO[0].GetContainerID() != "demo" || !tasks.closeIO[0].GetStdin() {
		t.Fatalf("CloseIO requests = %v, want one stdin close for demo", tasks.closeIO)
	}
}

type inputForwardingOperations struct {
	execOperations
	received chan string
}

func (o *inputForwardingOperations) ForwardTaskInput(_ context.Context, id string, input io.Reader) error {
	data, err := io.ReadAll(input)
	o.received <- id + ":" + string(data)
	return err
}

func TestAttachedCompatibilityStartForwardsInputUntilEOF(t *testing.T) {
	operations := &inputForwardingOperations{received: make(chan string, 1)}
	manager := New(&fakeRunner{outputs: map[string][]byte{}, errors: map[string]error{}})
	manager.execConnector = func(context.Context) (execOperations, error) {
		// Production lends the inventory's runtime through this wrapper.
		return borrowedExecOperations{operations}, nil
	}
	attachedErr := errors.New("container exited with code 3")
	started := newFakeProcess(func(input []byte) ([]byte, []byte, error) {
		if len(input) != 0 {
			return nil, nil, errors.New("nerdctl's ignored stdin received input")
		}
		return []byte("output"), nil, attachedErr
	})

	process := manager.forwardCompatibilityInput(context.Background(), "demo", started)
	output := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(process.Stdout()); output <- data }()
	if _, err := process.Stdin().Write([]byte("piped")); err != nil {
		t.Fatal(err)
	}
	if err := process.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-operations.received:
		if got != "demo:piped" {
			t.Fatalf("forwarded input = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("input EOF did not reach the task")
	}
	if got := <-output; string(got) != "output" {
		t.Fatalf("attached output = %q", got)
	}
	if err := process.Wait(); !errors.Is(err, attachedErr) {
		t.Fatalf("Wait = %v, want the attached start's exit", err)
	}
}
