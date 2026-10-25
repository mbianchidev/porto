package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type testWindowsImage struct{ containerd.Image }

func (testWindowsImage) Config(context.Context) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{Digest: digest.Digest("sha256:" + strings.Repeat("3", 64))}, nil
}

type testWindowsStore struct {
	mu          sync.Mutex
	values      map[string]*testWindowsContainer
	plans       []windowsContainerPlan
	tasks       int
	starts      int
	deletes     int
	failNewTask bool
	failStart   bool
	execs       int
}

func (s *testWindowsStore) List(ctx context.Context) ([]containerd.Container, error) {
	if namespace, _ := namespaces.Namespace(ctx); namespace != windowscontainers.Namespace {
		return nil, errors.New("escaped native namespace")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []containerd.Container
	for _, container := range s.values {
		result = append(result, container)
	}
	return result, nil
}
func (s *testWindowsStore) Create(ctx context.Context, id string, plan windowsContainerPlan) (containerd.Container, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plans = append(s.plans, plan)
	encoded, err := marshalWindowsSpec(plan.Spec)
	if err != nil {
		return nil, err
	}
	container := &testWindowsContainer{id: id, store: s, record: corecontainers.Container{ID: id, Labels: cloneStringMap(plan.Labels), Spec: encoded, CreatedAt: time.Now().UTC()}, spec: cloneWindowsTestSpec(plan.Spec)}
	s.values[id] = container
	return container, nil
}
func (s *testWindowsStore) UpdateSpec(_ context.Context, c containerd.Container, spec *specs.Spec) error {
	value := c.(*testWindowsContainer)
	value.mu.Lock()
	defer value.mu.Unlock()
	value.spec = cloneWindowsTestSpec(spec)
	return nil
}
func cloneWindowsTestSpec(value *specs.Spec) *specs.Spec {
	data, _ := json.Marshal(value)
	var result specs.Spec
	_ = json.Unmarshal(data, &result)
	return &result
}

type testWindowsContainer struct {
	containerd.Container
	id               string
	store            *testWindowsStore
	mu               sync.Mutex
	record           corecontainers.Container
	spec             *specs.Spec
	task             *testWindowsTask
	specErr          error
	nilSpec          bool
	specReads        int
	failSpecReadAt   int
	labelErrorState  string
	labelErr         error
	networkLabelErr  error
	labelReadErr     error
	deleteProcessErr error
}

func (c *testWindowsContainer) ID() string { return c.id }
func (c *testWindowsContainer) Info(context.Context, ...containerd.InfoOpts) (corecontainers.Container, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record := c.record
	record.Labels = cloneStringMap(c.record.Labels)
	return record, nil
}
func (c *testWindowsContainer) Labels(context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.labelReadErr != nil {
		return nil, c.labelReadErr
	}
	return cloneStringMap(c.record.Labels), nil
}
func (c *testWindowsContainer) SetLabels(_ context.Context, labels map[string]string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state, ok := labels[windowsStateLabel]; ok && state == c.labelErrorState && c.labelErr != nil {
		return nil, c.labelErr
	}
	if _, ok := labels[windowsNetworkLabel]; ok && c.networkLabelErr != nil {
		return nil, c.networkLabelErr
	}
	for k, v := range labels {
		c.record.Labels[k] = v
	}
	return cloneStringMap(c.record.Labels), nil
}
func (c *testWindowsContainer) Spec(context.Context) (*specs.Spec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.specReads++
	if c.specErr != nil && (c.failSpecReadAt == 0 || c.specReads >= c.failSpecReadAt) {
		return nil, c.specErr
	}
	if c.nilSpec {
		return nil, nil
	}
	return cloneWindowsTestSpec(c.spec), nil
}
func (c *testWindowsContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.task == nil {
		return nil, errdefs.ErrNotFound
	}
	c.task.process.mu.Lock()
	defer c.task.process.mu.Unlock()
	if c.task.process.deleted {
		return nil, errdefs.ErrNotFound
	}
	return c.task, nil
}
func (c *testWindowsContainer) NewTask(ctx context.Context, creator cio.Creator, _ ...containerd.NewTaskOpts) (containerd.Task, error) {
	c.store.mu.Lock()
	c.store.tasks++
	fail := c.store.failNewTask
	c.store.mu.Unlock()
	if fail {
		return nil, errors.New("synthetic runhcs NewTask failure")
	}
	ioSet, err := creator(c.id)
	if err != nil {
		return nil, err
	}
	spec, _ := c.Spec(ctx)
	process := &testWindowsProcess{id: c.id, hub: ioSet.(*windowsCIO).hub, container: c, init: true, size: [2]uint32{32, 120}, done: make(chan struct{})}
	if spec.Process.ConsoleSize != nil {
		process.size = [2]uint32{uint32(spec.Process.ConsoleSize.Height), uint32(spec.Process.ConsoleSize.Width)}
	}
	task := &testWindowsTask{process: process}
	c.mu.Lock()
	c.task = task
	c.mu.Unlock()
	return task, nil
}
func (c *testWindowsContainer) Delete(context.Context, ...containerd.DeleteOpts) error {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	delete(c.store.values, c.id)
	c.store.deletes++
	return nil
}

type testWindowsTask struct {
	containerd.Task
	process *testWindowsProcess
}

func (p *testWindowsTask) ID() string                      { return p.process.ID() }
func (p *testWindowsTask) Pid() uint32                     { return p.process.Pid() }
func (p *testWindowsTask) Start(ctx context.Context) error { return p.process.Start(ctx) }
func (p *testWindowsTask) Delete(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	return p.process.Delete(ctx, opts...)
}
func (p *testWindowsTask) Kill(ctx context.Context, signal syscall.Signal, opts ...containerd.KillOpts) error {
	return p.process.Kill(ctx, signal, opts...)
}
func (p *testWindowsTask) Wait(ctx context.Context) (<-chan containerd.ExitStatus, error) {
	return p.process.Wait(ctx)
}
func (p *testWindowsTask) CloseIO(ctx context.Context, opts ...containerd.IOCloserOpts) error {
	return p.process.CloseIO(ctx, opts...)
}
func (p *testWindowsTask) Resize(ctx context.Context, w, h uint32) error {
	return p.process.Resize(ctx, w, h)
}
func (p *testWindowsTask) Status(ctx context.Context) (containerd.Status, error) {
	return p.process.Status(ctx)
}
func (p *testWindowsTask) IO() cio.IO { return &windowsCIO{hub: p.process.hub} }
func (p *testWindowsTask) Exec(_ context.Context, id string, spec *specs.Process, creator cio.Creator) (containerd.Process, error) {
	p.process.container.store.mu.Lock()
	p.process.container.store.execs++
	p.process.container.store.mu.Unlock()
	ioSet, err := creator(id)
	if err != nil {
		return nil, err
	}
	process := &testWindowsProcess{id: id, hub: ioSet.(*windowsCIO).hub, container: p.process.container, done: make(chan struct{}), args: append([]string(nil), spec.Args...), size: [2]uint32{32, 120}}
	if spec.ConsoleSize != nil {
		process.size = [2]uint32{uint32(spec.ConsoleSize.Height), uint32(spec.ConsoleSize.Width)}
	}
	return process, nil
}

type testWindowsProcess struct {
	containerd.Process
	id        string
	container *testWindowsContainer
	hub       *windowsIOHub
	mu        sync.Mutex
	started   bool
	stopped   bool
	deleted   bool
	init      bool
	code      uint32
	done      chan struct{}
	size      [2]uint32
	args      []string
}

func (p *testWindowsProcess) ID() string  { return p.id }
func (p *testWindowsProcess) Pid() uint32 { return 1234 }
func (p *testWindowsProcess) Start(context.Context) error {
	p.container.store.mu.Lock()
	p.container.store.starts++
	fail := p.container.store.failStart
	p.container.store.mu.Unlock()
	if fail {
		return errors.New("synthetic runhcs Start failure")
	}
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	if p.init && !p.hub.config.Terminal {
		_, _ = p.hub.writer("stdout").Write([]byte("native-ready\n"))
		return nil
	}
	if !p.init && !p.hub.config.Terminal {
		if len(p.args) > 0 && p.args[0] == "fail" {
			p.complete(37)
			return nil
		}
		_, _ = p.hub.writer("stdout").Write([]byte("native-exec-stdout\n"))
		_, _ = p.hub.writer("stderr").Write([]byte("native-exec-stderr\n"))
		p.complete(0)
		return nil
	}
	_, _ = p.hub.writer("stdout").Write([]byte("native-tty-ready\n"))
	go func() {
		scanner := bufio.NewScanner(p.hub.inputReader)
		for scanner.Scan() {
			command := strings.TrimSpace(scanner.Text())
			if command == "size" {
				p.mu.Lock()
				size := p.size
				p.mu.Unlock()
				_, _ = p.hub.writer("stdout").Write([]byte(fmt.Sprintf("native-tty-size:%dx%d\n", size[1], size[0])))
			}
			if command == "exit" {
				_, _ = p.hub.writer("stdout").Write([]byte("native-tty-final-output\n"))
				p.complete(37)
				return
			}
		}
	}()
	return nil
}
func (p *testWindowsProcess) complete(code uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	p.code = code
	close(p.done)
}
func (p *testWindowsProcess) Delete(context.Context, ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error) {
	p.container.mu.Lock()
	deleteErr := p.container.deleteProcessErr
	p.container.mu.Unlock()
	if deleteErr != nil {
		return nil, deleteErr
	}
	p.complete(137)
	p.mu.Lock()
	p.deleted = true
	code := p.code
	p.mu.Unlock()
	return containerd.NewExitStatus(code, time.Now(), nil), nil
}
func (p *testWindowsProcess) Kill(context.Context, syscall.Signal, ...containerd.KillOpts) error {
	p.complete(137)
	return nil
}
func (p *testWindowsProcess) Wait(context.Context) (<-chan containerd.ExitStatus, error) {
	channel := make(chan containerd.ExitStatus, 1)
	go func() {
		<-p.done
		p.mu.Lock()
		code := p.code
		p.mu.Unlock()
		channel <- *containerd.NewExitStatus(code, time.Now(), nil)
		close(channel)
	}()
	return channel, nil
}
func (p *testWindowsProcess) CloseIO(context.Context, ...containerd.IOCloserOpts) error { return nil }
func (p *testWindowsProcess) Resize(_ context.Context, w, h uint32) error {
	p.mu.Lock()
	p.size = [2]uint32{h, w}
	p.mu.Unlock()
	return nil
}
func (p *testWindowsProcess) Status(context.Context) (containerd.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := containerd.Created
	if p.started {
		state = containerd.Running
	}
	if p.stopped {
		state = containerd.Stopped
	}
	return containerd.Status{Status: state, ExitStatus: p.code}, nil
}

func nativeEngineFixture(t *testing.T) (*Manager, *windowsEngine, *testWindowsStore) {
	t.Helper()
	runner := &fakeRunner{}
	manager := nativeTestManager(t, runner)
	runner.handler = func(command runtimes.Command) ([]byte, error) {
		args := nativeCommandArguments(t, manager, command)
		if len(args) > 0 && args[0] == "image" {
			return []byte(`[{"Id":"sha256:` + strings.Repeat("3", 64) + `","Descriptor":{"digest":"sha256:` + strings.Repeat("1", 64) + `"},"Os":"windows","Architecture":"amd64","OsVersion":"10.0.26100.1"}]`), nil
		}
		return nil, fmt.Errorf("native workload must not invoke nerdctl %v", args)
	}
	store := &testWindowsStore{values: make(map[string]*testWindowsContainer)}
	engine := &windowsEngine{manager: manager, store: store, sessions: make(map[string]*windowsSession), locks: newContainerMutexes()}
	engine.image = func(context.Context, string) (containerd.Image, ocispec.Image, error) {
		return testWindowsImage{}, ocispec.Image{
			Platform: ocispec.Platform{OS: "windows", Architecture: "amd64", OSVersion: "10.0.26100.1"},
			Config:   ocispec.ImageConfig{Cmd: []string{"cmd.exe"}, User: "ContainerUser", Env: []string{`Path=C:\Windows\System32`}}}, nil
	}
	engine.network = func(_ context.Context, id string, request CreateContainerRequest) (windowscontainers.Attachment, error) {
		return windowscontainers.Attachment{
			Container: id, Network: request.Networks[0].Name, Namespace: "owned-hcn-namespace", NetworkID: "owned-hns", Address: "10.253.1.2", Gateway: "10.253.1.1"}, nil
	}
	engine.removeNetwork = func(context.Context, windowscontainers.Attachment) error { return nil }
	engine.newIO = func(_ string, tty, stdin bool, path string) (*windowsIOHub, error) {
		return memoryWindowsIO(tty, stdin, path)
	}
	manager.windows.driver = engine
	t.Cleanup(func() {
		engine.mu.Lock()
		sessions := make([]*windowsSession, 0, len(engine.sessions))
		for _, session := range engine.sessions {
			sessions = append(sessions, session)
		}
		engine.mu.Unlock()
		for _, session := range sessions {
			_ = killWindowsProcess(context.Background(), session.process)
			<-session.done
		}
	})
	return manager, engine, store
}

func createNativeEngineTestContainer(t *testing.T, manager *Manager, tty bool) string {
	t.Helper()
	id, err := manager.CreateContainer(context.Background(), CreateContainerRequest{Name: "native-fixture", Image: "synthetic", TTY: tty, Interactive: tty,
		ConsoleSize: [2]uint{30, 90}, Command: []string{"native-fixture.exe"}, Isolation: "process"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNativeWindowsContainersCreateOnlyPreparesMetadataAndWindowsSnapshot(t *testing.T) {
	manager, _, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	if store.tasks != 0 || store.starts != 0 {
		t.Fatal("Docker create executed a native workload")
	}
	plan := store.plans[0]
	if plan.Request.Image != "sha256:"+strings.Repeat("1", 64) {
		t.Fatalf("create did not resolve the config ID to manifest: %s", plan.Request.Image)
	}
	if plan.Spec.Windows == nil || plan.Spec.Linux != nil || plan.Spec.Windows.Network != nil && plan.Spec.Windows.Network.NetworkNamespace != "" {
		t.Fatal("create adopted Linux spec or allocated a task network")
	}
	if plan.Snapshot != "porto-windows-"+id {
		t.Fatalf("unowned native snapshot %s", plan.Snapshot)
	}
	document, err := manager.InspectContainer(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(document), `"Running":false`) || !strings.Contains(string(document), `"Status":"created"`) {
		t.Fatalf("created native inspect=%s", document)
	}
	if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
		t.Fatal(err)
	}
	if store.tasks != 1 || store.starts != 1 {
		t.Fatal("explicit start did not create exactly one runhcs task")
	}
	if err := manager.ContainerAction(context.Background(), id, "remove"); !errors.Is(err, ErrConflict) {
		t.Fatalf("remove interrupted native workload: %v", err)
	}
}

func TestNativeWindowsContainersRunhcsLifecyclePreservesCreatedData(t *testing.T) {
	manager, _, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	for attempt := 0; attempt < 2; attempt++ {
		if err := manager.ContainerAction(context.Background(), id, "start"); err != nil {
			t.Fatal(err)
		}
		if err := manager.ContainerAction(context.Background(), id, "stop"); err != nil {
			t.Fatal(err)
		}
	}
	if store.tasks != 2 || store.starts != 2 || len(store.values) != 1 {
		t.Fatalf("native restart lost metadata or reused deleted task: %+v", store)
	}
	if err := manager.ContainerAction(context.Background(), id, "remove"); err != nil {
		t.Fatal(err)
	}
	if len(store.values) != 0 {
		t.Fatal("owned native metadata was not removed")
	}
}

func TestNativeWindowsContainersInvalidCreateCannotReachSnapshotOrTask(t *testing.T) {
	manager, _, store := nativeEngineFixture(t)
	_, err := manager.CreateContainer(context.Background(), CreateContainerRequest{Image: "synthetic", Isolation: "host"})
	if err == nil || len(store.plans) != 0 || store.tasks != 0 {
		t.Fatalf("invalid native isolation mutated the runtime: %v", err)
	}
}

func TestNativeWindowsContainersRunhcsFailureRollsBackOnlyItsEndpoint(t *testing.T) {
	manager, engine, store := nativeEngineFixture(t)
	id := createNativeEngineTestContainer(t, manager, false)
	var removed []windowscontainers.Attachment
	engine.removeNetwork = func(_ context.Context, attachment windowscontainers.Attachment) error {
		removed = append(removed, attachment)
		return nil
	}
	store.failNewTask = true
	if err := manager.ContainerAction(context.Background(), id, "start"); err == nil {
		t.Fatal("failed HCS create reported success")
	}
	if len(removed) != 1 || removed[0].Container != id || len(store.values) != 1 {
		t.Fatalf("partial native task cleanup escaped its endpoint or removed data: %+v", removed)
	}
}
