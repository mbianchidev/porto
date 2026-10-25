package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestNativeWindowsContainersPersistedMetadataErrorsAreVisible(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(map[string]string)
	}{
		{"request-json", func(labels map[string]string) { labels[windowsRequestLabel] = "{" }},
		{"request-null", func(labels map[string]string) { labels[windowsRequestLabel] = "null" }},
		{"attachment-json", func(labels map[string]string) { labels[windowsNetworkLabel] = "{" }},
		{"attachment-null", func(labels map[string]string) { labels[windowsNetworkLabel] = "null" }},
		{"ports", func(labels map[string]string) {
			var request CreateContainerRequest
			_ = json.Unmarshal([]byte(labels[windowsRequestLabel]), &request)
			request.Publish = []string{"invalid-port"}
			data, _ := json.Marshal(request)
			labels[windowsRequestLabel] = string(data)
		}},
		{"exit-code", func(labels map[string]string) { labels["io.porto.windows.exit-code"] = "not-a-number" }},
		{"exit-overflow", func(labels map[string]string) { labels["io.porto.windows.exit-code"] = "4294967296" }},
		{"exited-missing-code", func(labels map[string]string) {
			labels[windowsStateLabel] = "exited"
			delete(labels, "io.porto.windows.exit-code")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _, store := nativeEngineFixture(t)
			id := createNativeEngineTestContainer(t, manager, false)
			container := store.values[id]
			container.mu.Lock()
			test.edit(container.record.Labels)
			labels := cloneStringMap(container.record.Labels)
			container.mu.Unlock()
			if _, err := manager.InspectContainer(context.Background(), id); err == nil {
				t.Fatal("corrupt native inspect metadata was hidden")
			}
			value := Container{ID: id}
			if err := applyWindowsContainerMetadata(&value, labels, false); err == nil {
				t.Fatal("corrupt native inventory metadata was hidden")
			}
			data, _ := json.Marshal(&specs.Spec{Windows: &specs.Windows{}, Process: &specs.Process{}})
			mapped := containerFromContainerd(&containersapi.Container{ID: id, Labels: labels, Spec: &anypb.Any{Value: data}}, nil)
			if mapped.InventoryError == "" {
				t.Fatal("native metadata failure did not reach inventory mapping errors")
			}
		})
	}
}

func TestNativeWindowsContainersLinuxUserLabelsNeverSelectWindowsMapping(t *testing.T) {
	for _, withWindows := range []bool{false, true} {
		spec := &specs.Spec{Linux: &specs.Linux{}, Process: &specs.Process{Args: []string{"echo", "linux"}}}
		if withWindows {
			spec.Windows = &specs.Windows{}
		}
		data, _ := json.Marshal(spec)
		value := containerFromContainerd(&containersapi.Container{ID: "linux-container", Image: "linux-image",
			Labels: map[string]string{windowsRequestLabel: "malformed user label", windowsOwnerLabel: "untrusted", windowsStateLabel: "exited", "io.porto.windows.exit-code": "bad"},
			Spec:   &anypb.Any{Value: data}}, nil)
		if value.Image != "linux-image" || value.Command != "echo linux" || value.InventoryError != "" || value.ExitCode != nil {
			t.Fatalf("Linux user labels reinterpreted as native Windows: %+v", value)
		}
	}
}

func TestNativeWindowsContainersSpecReadErrorsAndMissingProcessFail(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*testWindowsContainer)
	}{
		{"read", func(c *testWindowsContainer) { c.specErr = errors.New("synthetic OCI read failure") }},
		{"nil-spec", func(c *testWindowsContainer) { c.nilSpec = true }},
		{"nil-process", func(c *testWindowsContainer) { c.spec.Process = nil }},
		{"inventory-second-read", func(c *testWindowsContainer) {
			c.specErr = errors.New("synthetic inventory OCI read failure")
			c.failSpecReadAt = 2
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _, store := nativeEngineFixture(t)
			id := createNativeEngineTestContainer(t, manager, false)
			c := store.values[id]
			c.mu.Lock()
			test.set(c)
			c.mu.Unlock()
			if test.name == "inventory-second-read" {
				if _, err := manager.Containers(context.Background()); err == nil {
					t.Fatal("native inventory ignored spec read failure")
				}
			} else if _, err := manager.InspectContainer(context.Background(), id); err == nil {
				t.Fatal("native inspect ignored absent process/spec failure")
			}
		})
	}
}

func TestNativeWindowsContainersEnvironmentValidationAndCaseInsensitiveOverride(t *testing.T) {
	value, err := windowsEnvironment([]string{"Path=original", "KEEP=yes"}, []string{"pATH=override", "EMPTY=", "VALUE=one=two"})
	if err != nil || strings.Join(value, "|") != "pATH=override|KEEP=yes|EMPTY=|VALUE=one=two" {
		t.Fatalf("Windows environment merge=%v, %v", value, err)
	}
	for _, entry := range []string{"BROKEN", "=bad", "", "KEY=\x00", "N\x00AME=value"} {
		if _, err := windowsEnvironment(nil, []string{entry}); err == nil {
			t.Fatalf("invalid native environment accepted: %q", entry)
		}
		manager, engine, store := nativeEngineFixture(t)
		if _, err := manager.CreateContainer(context.Background(), CreateContainerRequest{Image: "synthetic", Environment: []string{entry}}); err == nil || len(store.plans) != 0 {
			t.Fatalf("invalid native create environment mutated snapshot metadata: %q, %v", entry, err)
		}
		id := createNativeEngineTestContainer(t, manager, false)
		if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Exec(context.Background(), ExecRequest{ContainerID: id, Command: []string{"fixture"}, Environment: []string{entry}}); err == nil || store.execs != 0 {
			t.Fatalf("invalid native exec environment created a process: %q, %v", entry, err)
		}
	}
}

type inputErrorDriver struct {
	windowsDriver
	process runtimes.Process
}

func (d inputErrorDriver) Exec(context.Context, ExecRequest) (runtimes.Process, error) {
	return d.process, nil
}

type errorNativeInput struct {
	writeErr, closeErr error
	short              bool
}

func (i errorNativeInput) Write(data []byte) (int, error) {
	if i.short {
		return len(data) - 1, nil
	}
	return 0, i.writeErr
}
func (i errorNativeInput) Close() error { return i.closeErr }

type inputErrorProcess struct {
	input            io.WriteCloser
	killErr, waitErr error
	done             chan struct{}
	once             sync.Once
	waited, killed   bool
}

func (p *inputErrorProcess) Stdin() io.WriteCloser { return p.input }
func (p *inputErrorProcess) Stdout() io.ReadCloser { return io.NopCloser(strings.NewReader("stdout")) }
func (p *inputErrorProcess) Stderr() io.ReadCloser { return io.NopCloser(strings.NewReader("stderr")) }
func (p *inputErrorProcess) PID() int              { return 1 }
func (p *inputErrorProcess) Kill() error {
	p.killed = true
	p.once.Do(func() { close(p.done) })
	return p.killErr
}
func (p *inputErrorProcess) Wait() error { <-p.done; p.waited = true; return p.waitErr }

func TestNativeWindowsContainersExecContainerInputFailuresAreNotSuccess(t *testing.T) {
	writeErr, closeErr, killErr, waitErr := errors.New("write failed"), errors.New("close failed"), errors.New("kill cleanup failed"), errors.New("wait cleanup failed")
	manager := nativeTestManager(t, &fakeRunner{})
	process := &inputErrorProcess{input: errorNativeInput{writeErr: writeErr, closeErr: closeErr}, killErr: killErr, waitErr: waitErr, done: make(chan struct{})}
	manager.windows.driver = inputErrorDriver{process: process}
	output, err := manager.ExecContainer(context.Background(), "owned", []string{"fixture"}, []byte("payload"))
	if string(output) != "stdoutstderr" || !errors.Is(err, writeErr) || !errors.Is(err, closeErr) || !errors.Is(err, killErr) || !errors.Is(err, waitErr) || !process.killed || !process.waited {
		t.Fatalf("native stdin/cleanup failure was hidden: %q, %v", output, err)
	}
	short := &inputErrorProcess{input: errorNativeInput{short: true}, done: make(chan struct{})}
	manager.windows.driver = inputErrorDriver{process: short}
	if _, err := manager.ExecContainer(context.Background(), "owned", []string{"fixture"}, []byte("payload")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("native short stdin transfer=%v", err)
	}
}

func TestNativeWindowsContainersStartStatePersistenceFailurePreservesLiveTask(t *testing.T) {
	manager, engine, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	sentinel := errors.New("persist running labels failed")
	container := store.values[id]
	container.mu.Lock()
	container.labelErrorState = "running"
	container.labelErr = sentinel
	container.mu.Unlock()
	var mu sync.Mutex
	removed := 0
	engine.removeNetwork = func(context.Context, windowscontainers.Attachment) error {
		mu.Lock()
		removed++
		mu.Unlock()
		return nil
	}
	if err := manager.ContainerAction(context.Background(), id, "start"); !errors.Is(err, sentinel) {
		t.Fatalf("successful task start hid persistence failure: %v", err)
	}
	engine.mu.Lock()
	session := engine.sessions[id]
	engine.mu.Unlock()
	if session == nil {
		t.Fatal("live native task session lost after label failure")
	}
	state, err := session.process.Status(context.Background())
	if err != nil || state.Status != "running" {
		t.Fatal("label failure stopped a successfully started native workload")
	}
	mu.Lock()
	count := removed
	mu.Unlock()
	if count != 0 {
		t.Fatal("label failure tore down a live task endpoint")
	}
	container.mu.Lock()
	container.labelErr = nil
	container.mu.Unlock()
	container.task.process.complete(23)
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("preserved task observer did not reap its exit")
	}
	if session.code != 23 || session.err != nil {
		t.Fatalf("native task observer lost exit: %d, %v", session.code, session.err)
	}
}

func TestNativeWindowsContainersWatchPersistenceAndCleanupErrorsRemainVisible(t *testing.T) {
	for _, kind := range []string{"exit-label", "task-delete", "auto-remove-read"} {
		t.Run(kind, func(t *testing.T) {
			manager, engine, store := nativeEngineFixture(t)
			id := createNativeEngineTestContainer(t, manager, false)
			if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
				t.Fatal(err)
			}
			engine.mu.Lock()
			session := engine.sessions[id]
			engine.mu.Unlock()
			sentinel := errors.New("synthetic native " + kind + " failure")
			c := store.values[id]
			c.mu.Lock()
			switch kind {
			case "exit-label":
				c.labelErrorState = "exited"
				c.labelErr = sentinel
			case "task-delete":
				c.deleteProcessErr = sentinel
			case "auto-remove-read":
				c.labelReadErr = sentinel
			}
			c.mu.Unlock()
			var mu sync.Mutex
			removed := 0
			engine.removeNetwork = func(context.Context, windowscontainers.Attachment) error {
				mu.Lock()
				removed++
				mu.Unlock()
				return nil
			}
			c.task.process.complete(11)
			select {
			case <-session.done:
			case <-time.After(time.Second):
				t.Fatal("native failure observer did not complete")
			}
			if !errors.Is(session.err, sentinel) {
				t.Fatalf("native watch discarded %s error: %v", kind, session.err)
			}
			engine.mu.Lock()
			kept := engine.sessions[id] == session
			engine.mu.Unlock()
			if !kept {
				t.Fatal("failed native cleanup lost its recovery session")
			}
			mu.Lock()
			count := removed
			mu.Unlock()
			if kind != "auto-remove-read" && count != 0 {
				t.Fatal("failed task/state cleanup removed endpoint without a recoverable task state")
			}
		})
	}
}

func TestNativeWindowsContainersPreStartRollbackErrorsAreJoined(t *testing.T) {
	manager, engine, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	labelsErr, networkErr, ioErr := errors.New("persist endpoint labels failed"), errors.New("endpoint rollback failed"), errors.New("I/O cleanup failed")
	container := store.values[id]
	container.networkLabelErr = labelsErr
	engine.removeNetwork = func(context.Context, windowscontainers.Attachment) error { return networkErr }
	if err := manager.ContainerAction(context.Background(), id, "start"); !errors.Is(err, labelsErr) || !errors.Is(err, networkErr) {
		t.Fatalf("native pre-start rollback error lost: %v", err)
	}
	container.networkLabelErr = nil
	store.failNewTask = true
	engine.newIO = func(_ string, tty, stdin bool, _ string) (*windowsIOHub, error) {
		hub, err := memoryWindowsIO(tty, stdin, "")
		if err == nil {
			hub.closeLog = func() error { return ioErr }
		}
		return hub, err
	}
	if err := manager.ContainerAction(context.Background(), id, "start"); err == nil || !errors.Is(err, ioErr) || !errors.Is(err, networkErr) {
		t.Fatalf("native task failure lost actual cleanup errors: %v", err)
	}
}
