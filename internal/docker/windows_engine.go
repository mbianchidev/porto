package docker

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	corecontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const windowsRequestLabel = "io.porto.windows.request"
const windowsOwnerLabel = "io.porto.windows.owner"
const windowsStateLabel = "io.porto.windows.state"
const windowsNetworkLabel = "io.porto.windows.endpoint"
const windowsImageConfigLabel = "io.porto.windows.image-config"
const windowsImageVersionLabel = "io.porto.windows.image-version"

type windowsDriver interface {
	Create(context.Context, CreateContainerRequest) (string, error)
	Inspect(context.Context, string) (json.RawMessage, error)
	Containers(context.Context, bool) ([]Container, error)
	Action(context.Context, string, string, int) error
	Exec(context.Context, ExecRequest) (runtimes.Process, error)
	Attach(context.Context, string, bool, bool, [2]uint) (runtimes.Process, error)
	Logs(context.Context, string, LogOptions, func(runtimes.OutputChunk) error) error
	Console(context.Context, string, [2]uint, bool) error
}

type windowsContainerPlan struct {
	Request  CreateContainerRequest
	Spec     *specs.Spec
	Labels   map[string]string
	Image    containerd.Image
	Snapshot string
}

// The store keeps Windows SDK operations separate from the Linux OCI/FIFO runtime.
type windowsContainerStore interface {
	List(context.Context) ([]containerd.Container, error)
	Create(context.Context, string, windowsContainerPlan) (containerd.Container, error)
	UpdateSpec(context.Context, containerd.Container, *specs.Spec) error
}

type windowsSDKStore struct{ client *containerd.Client }

func (s windowsSDKStore) List(ctx context.Context) ([]containerd.Container, error) {
	return s.client.Containers(ctx)
}
func (s windowsSDKStore) Create(ctx context.Context, id string, plan windowsContainerPlan) (containerd.Container, error) {
	container, err := s.client.NewContainer(ctx, id,
		containerd.WithImage(plan.Image), containerd.WithContainerLabels(plan.Labels),
		containerd.WithRuntime(windowscontainers.RuntimeName, nil),
		containerd.WithSnapshotter(windowscontainers.Snapshotter),
		containerd.WithNewSnapshot(plan.Snapshot, plan.Image),
		containerd.WithSpec(plan.Spec))
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, statErr := s.client.SnapshotService(windowscontainers.Snapshotter).Stat(cleanup, plan.Snapshot); statErr == nil {
			err = errors.Join(err, s.client.SnapshotService(windowscontainers.Snapshotter).Remove(cleanup, plan.Snapshot))
		}
	}
	return container, err
}
func (s windowsSDKStore) UpdateSpec(ctx context.Context, container containerd.Container, spec *specs.Spec) error {
	return container.Update(ctx, func(_ context.Context, _ *containerd.Client, record *corecontainers.Container) error {
		encoded, err := marshalWindowsSpec(spec)
		if err != nil {
			return err
		}
		record.Spec = encoded
		return nil
	})
}

type windowsEngine struct {
	manager       *Manager
	store         windowsContainerStore
	image         func(context.Context, string) (containerd.Image, ocispec.Image, error)
	network       func(context.Context, string, CreateContainerRequest) (windowscontainers.Attachment, error)
	removeNetwork func(context.Context, windowscontainers.Attachment) error
	newIO         func(string, bool, bool, string) (*windowsIOHub, error)
	mu            sync.Mutex
	sessions      map[string]*windowsSession
	locks         *containerMutexes
}

type windowsSession struct {
	process containerd.Process
	hub     *windowsIOHub
	done    chan struct{}
	code    uint32
	err     error
	started time.Time
}

func (m *Manager) nativeWindowsDriver(ctx context.Context) (windowsDriver, error) {
	m.windows.driverMu.Lock()
	defer m.windows.driverMu.Unlock()
	if m.windows.driver != nil {
		return m.windows.driver, nil
	}
	driver, err := m.windows.connectDriver(ctx)
	if err != nil {
		return nil, err
	}
	m.windows.driver = driver
	return driver, nil
}

func (m *Manager) connectWindowsDriver(ctx context.Context) (windowsDriver, error) {
	runtime, err := m.connectContainerRuntime(ctx)
	if err != nil {
		return nil, err
	}
	wrapped, ok := runtime.(windowsInventory)
	if !ok {
		_ = runtime.Close()
		return nil, fmt.Errorf("%w: native Windows containerd connection", windowscontainers.ErrOwnership)
	}
	client, ok := wrapped.containerRuntime.(*grpcContainerRuntime)
	if !ok || client.client == nil {
		_ = runtime.Close()
		return nil, ErrUnavailable
	}
	engine := &windowsEngine{manager: m, store: windowsSDKStore{client: client.client}, sessions: make(map[string]*windowsSession), locks: newContainerMutexes()}
	engine.image = func(ctx context.Context, target string) (containerd.Image, ocispec.Image, error) {
		images, err := client.client.ListImages(ctx)
		if err != nil {
			return nil, ocispec.Image{}, err
		}
		var selected containerd.Image
		for _, image := range images {
			if image.Target().Digest.String() == target {
				selected = image
				break
			}
		}
		if selected == nil {
			return nil, ocispec.Image{}, fmt.Errorf("%w: native image manifest %s", ErrNotFound, target)
		}
		config, err := selected.Spec(ctx)
		return selected, config, err
	}
	engine.network = func(ctx context.Context, id string, request CreateContainerRequest) (windowscontainers.Attachment, error) {
		return windowscontainers.AttachNetwork(ctx, m.windows.installation, id, request.Networks[0].Name, request.Publish)
	}
	engine.removeNetwork = func(ctx context.Context, attachment windowscontainers.Attachment) error {
		return windowscontainers.DetachNetwork(ctx, m.windows.installation, attachment)
	}
	engine.newIO = func(id string, tty, stdin bool, logPath string) (*windowsIOHub, error) {
		return newWindowsIOHub(m.windows.installation, id, tty, stdin, logPath)
	}
	return engine, nil
}

func windowsContext(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, windowscontainers.Namespace)
}

func (e *windowsEngine) find(ctx context.Context, id string) (containerd.Container, CreateContainerRequest, error) {
	containers, err := e.store.List(windowsContext(ctx))
	if err != nil {
		return nil, CreateContainerRequest{}, err
	}
	var selected containerd.Container
	var request CreateContainerRequest
	for _, container := range containers {
		labels, err := container.Labels(windowsContext(ctx))
		if err != nil {
			return nil, request, err
		}
		if container.ID() != id && labels[nerdctlNameLabel] != id && !strings.HasPrefix(container.ID(), id) {
			continue
		}
		if labels[windowsOwnerLabel] != e.manager.windows.installation.OwnerSID || labels[windowsRequestLabel] == "" {
			return nil, request, fmt.Errorf("%w: native container %s is not owned by this worker", windowscontainers.ErrOwnership, id)
		}
		if selected != nil && container.ID() != id && labels[nerdctlNameLabel] != id {
			return nil, request, fmt.Errorf("%w: ambiguous native container ID", ErrConflict)
		}
		request, err = persistedWindowsRequest(container.ID(), labels)
		if err != nil {
			return nil, request, err
		}
		selected = container
		if container.ID() == id || labels[nerdctlNameLabel] == id {
			break
		}
	}
	if selected == nil {
		return nil, request, fmt.Errorf("%w: no such container: %s", ErrNotFound, id)
	}
	return selected, request, nil
}

func (e *windowsEngine) Create(ctx context.Context, request CreateContainerRequest) (string, error) {
	ctx = windowsContext(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if request.Name != "" {
		if _, _, err := e.find(ctx, request.Name); err == nil {
			return "", fmt.Errorf("%w: native container name already exists", ErrConflict)
		} else if !errors.Is(err, ErrNotFound) {
			return "", err
		}
	}
	image, config, err := e.image(ctx, request.Image)
	if err != nil {
		return "", err
	}
	if err := e.manager.windows.installation.Host.ValidateImage(windowscontainers.ImagePlatform{
		OS: config.OS, Architecture: config.Architecture, OSVersion: config.OSVersion}, request.Isolation); err != nil {
		return "", err
	}
	id, err := randomWindowsContainerID()
	if err != nil {
		return "", err
	}
	spec, err := windowsContainerSpec(ctx, request, config)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	labels := cloneStringMap(request.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[windowsRequestLabel] = string(encoded)
	labels[windowsOwnerLabel] = e.manager.windows.installation.OwnerSID
	labels[nerdctlNameLabel] = firstNonEmpty(request.Name, id)
	labels[windowsStateLabel] = "created"
	labels[windowsImageVersionLabel] = config.OSVersion
	labels[portoTTYLabel] = strconv.FormatBool(request.TTY)
	labels[portoOpenStdinLabel] = strconv.FormatBool(request.Interactive)
	labels[portoAutoRemoveLabel] = strconv.FormatBool(request.Remove)
	labels[portoLogPathLabel] = filepath.Join(e.manager.stateDir, "container-logs", id+".log")
	labels[portoLogJournalLabel] = labels[portoLogPathLabel] + ".jsonl"
	console, _ := json.Marshal(request.ConsoleSize)
	labels["io.porto.container.console-size"] = string(console)
	configDesc, err := image.Config(ctx)
	if err != nil {
		return "", err
	}
	labels[windowsImageConfigLabel] = configDesc.Digest.String()
	_, err = e.store.Create(ctx, id, windowsContainerPlan{Request: request, Image: image, Spec: spec, Labels: labels, Snapshot: "porto-windows-" + id})
	if err != nil {
		return "", fmt.Errorf("create native Windows metadata and snapshot without starting: %w", err)
	}
	return id, nil
}

func windowsContainerSpec(ctx context.Context, request CreateContainerRequest, image ocispec.Image) (*specs.Spec, error) {
	record := &corecontainers.Container{ID: "porto-windows"}
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, "windows/amd64", record)
	if err != nil {
		return nil, err
	}
	command := request.Command
	if len(command) == 0 {
		command = image.Config.Cmd
	}
	entrypoint := image.Config.Entrypoint
	if request.Entrypoint != nil {
		entrypoint = request.Entrypoint
	}
	args := append(append([]string(nil), entrypoint...), command...)
	if len(args) == 0 {
		return nil, errors.New("invalid native Windows container: image/command has no executable")
	}
	spec.Process.Args = args
	if image.Config.ArgsEscaped && request.Entrypoint == nil && (len(entrypoint) > 0 || len(request.Command) == 0) {
		spec.Process.CommandLine = args[0]
		if len(args) > 1 {
			spec.Process.CommandLine += " " + windowsCommandLine(args[1:])
		}
		spec.Process.Args = nil
	}
	spec.Process.Env, err = windowsEnvironment(image.Config.Env, request.Environment)
	if err != nil {
		return nil, err
	}
	spec.Process.Cwd = firstNonEmpty(request.WorkingDir, image.Config.WorkingDir, `C:\`)
	if !windowsPath(spec.Process.Cwd) {
		return nil, errors.New("invalid native Windows working directory")
	}
	spec.Process.User.Username = firstNonEmpty(request.User, image.Config.User, "ContainerUser")
	spec.Process.Terminal = request.TTY
	if request.TTY {
		height, width := windowsConsoleSize(request.ConsoleSize)
		spec.Process.ConsoleSize = &specs.Box{Height: uint(height), Width: uint(width)}
	}
	if request.Isolation == "hyperv" {
		if err := oci.WithWindowsHyperV(ctx, nil, record, spec); err != nil {
			return nil, err
		}
	}
	if err := oci.WithWindowsIgnoreFlushesDuringBoot()(ctx, nil, record, spec); err != nil {
		return nil, err
	}
	if err := oci.WithWindowNetworksAllowUnqualifiedDNSQuery()(ctx, nil, record, spec); err != nil {
		return nil, err
	}
	if request.Limits.Memory > 0 {
		if err := oci.WithMemoryLimit(uint64(request.Limits.Memory))(ctx, nil, record, spec); err != nil {
			return nil, err
		}
	}
	if request.Limits.NanoCPUs > 0 {
		if err := oci.WithWindowsCPUCount(uint64(request.Limits.NanoCPUs/1_000_000_000))(ctx, nil, record, spec); err != nil {
			return nil, err
		}
	}
	for _, value := range request.Volumes {
		source, target, mode, err := windowsVolumeParts(value)
		if err != nil {
			return nil, err
		}
		if !windowsPath(source) {
			return nil, fmt.Errorf("%w: native managed/anonymous volumes require explicit bind directories", ErrUnsupported)
		}
		options := []string{"rw"}
		if mode == "ro" {
			options = []string{"ro"}
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{Source: source, Destination: target, Options: options})
	}
	for _, value := range request.Mounts {
		fields, err := csv.NewReader(strings.NewReader(value)).Read()
		if err != nil {
			return nil, err
		}
		mount := specs.Mount{Options: []string{"rw"}}
		for _, field := range fields {
			name, value, _ := strings.Cut(field, "=")
			switch name {
			case "source", "src":
				mount.Source = value
			case "target", "dst", "destination":
				mount.Destination = value
			case "readonly", "ro":
				mount.Options = []string{"ro"}
			}
		}
		spec.Mounts = append(spec.Mounts, mount)
	}
	return spec, nil
}

func windowsEnvironment(base, overrides []string) ([]string, error) {
	for _, entries := range [][]string{base, overrides} {
		for index, entry := range entries {
			name, _, ok := strings.Cut(entry, "=")
			if !ok || name == "" || strings.ContainsRune(entry, 0) {
				return nil, fmt.Errorf("invalid native Windows environment entry %d: a nonempty name=value without NUL is required", index)
			}
		}
	}
	result := append([]string(nil), base...)
	for _, entry := range overrides {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, errors.New("invalid native Windows environment entry")
		}
		replaced := false
		for i, old := range result {
			key, _, _ := strings.Cut(old, "=")
			if strings.EqualFold(key, name) {
				result[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, entry)
		}
	}
	return result, nil
}
func windowsConsoleSize(size [2]uint) (uint32, uint32) {
	h, w := uint32(size[0]), uint32(size[1])
	if h == 0 {
		h = 32
	}
	if w == 0 {
		w = 120
	}
	return h, w
}

func (e *windowsEngine) start(ctx context.Context, container containerd.Container, request CreateContainerRequest) (result *windowsSession, err error) {
	id := container.ID()
	lock := e.locks.lock(id)
	defer lock()
	ctx = windowsContext(ctx)
	if err := e.manager.windows.installation.Host.Validate(); err != nil {
		return nil, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return nil, err
	}
	if err := e.manager.windows.installation.Host.ValidateImage(windowscontainers.ImagePlatform{
		OS: "windows", Architecture: "amd64", OSVersion: labels[windowsImageVersionLabel]}, request.Isolation); err != nil {
		return nil, fmt.Errorf("invalid native Windows start image: %w", err)
	}
	if spec == nil || spec.Windows == nil || spec.Linux != nil || spec.Process == nil ||
		(request.Isolation == "hyperv") != (spec.Windows.HyperV != nil) ||
		spec.Annotations["microsoft.com/hostprocess-container"] == "true" {
		return nil, fmt.Errorf("%w: native Windows OCI isolation or ownership changed", windowscontainers.ErrOwnership)
	}
	task, err := container.Task(ctx, nil)
	if err == nil {
		status, statusErr := task.Status(ctx)
		if statusErr != nil {
			return nil, statusErr
		}
		if status.Status != containerd.Stopped {
			return nil, fmt.Errorf("%w: native container is already running", ErrConflict)
		}
		if _, err := task.Delete(ctx); err != nil {
			return nil, err
		}
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}
	if labels[windowsNetworkLabel] != "" {
		previous, err := persistedWindowsAttachment(id, labels)
		if err != nil {
			return nil, err
		}
		if err := e.removeNetwork(ctx, previous); err != nil {
			return nil, err
		}
	}
	attachment, err := e.network(ctx, id, request)
	if err != nil {
		return nil, err
	}
	rollbackNetwork := true
	defer func() {
		if rollbackNetwork {
			cleanup, cancel := context.WithTimeout(windowsContext(context.Background()), 30*time.Second)
			defer cancel()
			if cleanupErr := e.removeNetwork(cleanup, attachment); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("roll back native container %s endpoint: %w", id, cleanupErr))
			}
		}
	}()
	if spec.Windows.Network == nil {
		spec.Windows.Network = &specs.WindowsNetwork{}
	}
	spec.Windows.Network.NetworkNamespace = attachment.Namespace
	if err := e.store.UpdateSpec(ctx, container, spec); err != nil {
		return nil, err
	}
	encoded, marshalErr := json.Marshal(attachment)
	if marshalErr != nil {
		return nil, marshalErr
	}
	if _, err := container.SetLabels(ctx, map[string]string{windowsNetworkLabel: string(encoded)}); err != nil {
		return nil, err
	}
	hub, err := e.newIO(id, request.TTY, request.Interactive, labels[portoLogPathLabel])
	if err != nil {
		return nil, err
	}
	task, err = container.NewTask(ctx, hub.creator())
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create native runhcs task: %w", err), closeWindowsTaskIO(hub))
	}
	wait, err := task.Wait(windowsContext(context.Background()))
	if err != nil {
		deleteErr := deleteWindowsTask(ctx, task)
		if deleteErr != nil {
			rollbackNetwork = false
			return nil, errors.Join(err, deleteErr)
		}
		return nil, errors.Join(err, closeWindowsTaskIO(hub))
	}
	session := &windowsSession{process: task, hub: hub, done: make(chan struct{}), started: time.Now().UTC()}
	e.mu.Lock()
	e.sessions[id] = session
	e.mu.Unlock()
	if err := task.Start(ctx); err != nil {
		deleteErr := deleteWindowsTask(ctx, task)
		if deleteErr != nil {
			rollbackNetwork = false
			go e.watch(container, session, wait, attachment, true)
			return session, errors.Join(fmt.Errorf("start native runhcs task: %w", err), deleteErr)
		}
		closeErr := closeWindowsTaskIO(hub)
		e.mu.Lock()
		delete(e.sessions, id)
		e.mu.Unlock()
		return nil, errors.Join(fmt.Errorf("start native runhcs task: %w", err), closeErr)
	}
	rollbackNetwork = false
	_, persistErr := container.SetLabels(ctx, map[string]string{windowsStateLabel: "running", "io.porto.windows.started": session.started.Format(time.RFC3339Nano)})
	go e.watch(container, session, wait, attachment, true)
	if persistErr != nil {
		return session, fmt.Errorf("native container %s started but state persistence failed; task and endpoint preserved for recovery: %w", id, persistErr)
	}
	return session, nil
}

func closeWindowsTaskIO(hub *windowsIOHub) error {
	if err := hub.Close(); err != nil {
		return fmt.Errorf("close native runhcs task I/O: %w", err)
	}
	return nil
}

func deleteWindowsTask(ctx context.Context, process containerd.Process) error {
	cleanup, cancel := context.WithTimeout(windowsContext(context.WithoutCancel(ctx)), 30*time.Second)
	defer cancel()
	_, err := process.Delete(cleanup, containerd.WithProcessKill)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete native runhcs task during rollback: %w", err)
	}
	return nil
}

func (e *windowsEngine) watch(container containerd.Container, session *windowsSession, wait <-chan containerd.ExitStatus, attachment windowscontainers.Attachment, init bool) {
	status, ok := <-wait
	if !ok {
		session.err = errors.New("native process wait closed without an exit status")
	} else {
		session.code, _, session.err = status.Result()
	}
	if session.err != nil {
		probe, cancel := context.WithTimeout(windowsContext(context.Background()), 10*time.Second)
		status, statusErr := session.process.Status(probe)
		cancel()
		if statusErr != nil || status.Status != containerd.Stopped {
			// A broken wait connection is not proof that a Windows workload
			// exited. Preserve its task, endpoint and snapshot for recovery.
			session.err = errors.Join(session.err, statusErr)
			close(session.done)
			return
		}
		session.code = status.ExitStatus
		session.err = nil
	}
	session.hub.finish()
	session.hub.mu.Lock()
	session.err = errors.Join(session.err, session.hub.err)
	session.hub.mu.Unlock()
	cleanup, cancel := context.WithTimeout(windowsContext(context.Background()), 30*time.Second)
	defer cancel()
	if init {
		_, err := container.SetLabels(cleanup, map[string]string{windowsStateLabel: "exited", "io.porto.windows.exit-code": strconv.FormatUint(uint64(session.code), 10),
			"io.porto.windows.finished": time.Now().UTC().Format(time.RFC3339Nano)})
		if err != nil {
			session.err = errors.Join(session.err, fmt.Errorf("persist native container %s exit state; stopped task and endpoint preserved for recovery: %w", container.ID(), err))
			close(session.done)
			return
		}
	}
	_, deleteErr := session.process.Delete(cleanup)
	if !errdefs.IsNotFound(deleteErr) {
		if deleteErr != nil {
			session.err = errors.Join(session.err, fmt.Errorf("delete exited native process %s: %w", session.process.ID(), deleteErr))
			close(session.done)
			return
		}
	}
	if init {
		if err := e.removeNetwork(cleanup, attachment); err != nil {
			session.err = errors.Join(session.err, fmt.Errorf("detach native container %s endpoint: %w", container.ID(), err))
		}
		labels, err := container.Labels(cleanup)
		if err != nil {
			session.err = errors.Join(session.err, fmt.Errorf("read native auto-remove state: %w", err))
		} else if labels[portoAutoRemoveLabel] == "true" && session.err == nil {
			session.err = container.Delete(cleanup, containerd.WithSnapshotCleanup)
		}
		e.mu.Lock()
		if session.err == nil && e.sessions[container.ID()] == session {
			delete(e.sessions, container.ID())
		}
		e.mu.Unlock()
	}
	close(session.done)
}

func (e *windowsEngine) Action(ctx context.Context, id, action string, timeout int) error {
	container, request, err := e.find(ctx, id)
	if err != nil {
		return err
	}
	ctx = windowsContext(ctx)
	if action == "start" {
		_, err := e.start(ctx, container, request)
		return err
	}
	if action == "restart" {
		if err := e.stop(ctx, container, timeout, false); err != nil {
			return err
		}
		_, err := e.start(ctx, container, request)
		return err
	}
	if action == "stop" {
		return e.stop(ctx, container, timeout, false)
	}
	if action == "kill" {
		return e.stop(ctx, container, 0, true)
	}
	if action == "pause" || action == "unpause" {
		return fmt.Errorf("%w: native Windows pause/resume is not part of the supported runhcs contract", ErrUnsupported)
	}
	if strings.HasPrefix(action, "remove") {
		force := strings.Contains(action, "force")
		if !force {
			if task, err := container.Task(ctx, nil); err == nil {
				status, err := task.Status(ctx)
				if err != nil {
					return err
				}
				if status.Status != containerd.Stopped {
					return fmt.Errorf("%w: native container is running", ErrConflict)
				}
			} else if !errdefs.IsNotFound(err) {
				return err
			}
		}
		if err := e.stop(ctx, container, 0, force); err != nil {
			return err
		}
		labels, err := container.Labels(ctx)
		if err != nil {
			return err
		}
		if labels[windowsNetworkLabel] != "" {
			attachment, err := persistedWindowsAttachment(container.ID(), labels)
			if err != nil {
				return err
			}
			if err := e.removeNetwork(ctx, attachment); err != nil {
				return err
			}
		}
		return container.Delete(ctx, containerd.WithSnapshotCleanup)
	}
	return fmt.Errorf("%w: native action %s", ErrUnsupported, action)
}

func (e *windowsEngine) stop(ctx context.Context, container containerd.Container, timeout int, force bool) error {
	ctx = windowsContext(ctx)
	task, err := container.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		e.mu.Lock()
		session := e.sessions[container.ID()]
		e.mu.Unlock()
		if session != nil {
			select {
			case <-session.done:
				return session.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	status, err := task.Status(ctx)
	if err != nil {
		return err
	}
	if status.Status == containerd.Stopped {
		e.mu.Lock()
		session := e.sessions[container.ID()]
		e.mu.Unlock()
		if session != nil {
			select {
			case <-session.done:
				return session.err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, err := task.Delete(ctx)
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !force && timeout < 0 {
		return fmt.Errorf("%w: container is running", ErrConflict)
	}
	wait, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	if err := task.Kill(ctx, signal, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	delay := 10 * time.Second
	if timeout > 0 {
		delay = time.Duration(timeout) * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
		if err := task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	e.mu.Lock()
	session := e.sessions[container.ID()]
	e.mu.Unlock()
	if session != nil {
		select {
		case <-session.done:
			return session.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err = task.Delete(ctx)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *windowsEngine) Console(ctx context.Context, id string, size [2]uint, resize bool) error {
	container, _, err := e.find(ctx, id)
	if err != nil {
		return err
	}
	ctx = windowsContext(ctx)
	spec, err := container.Spec(ctx)
	if err != nil {
		return err
	}
	if spec == nil || spec.Process == nil || !spec.Process.Terminal {
		return errors.New("invalid native container console: container does not use a TTY")
	}
	height, width := windowsConsoleSize(size)
	if resize {
		task, err := container.Task(ctx, nil)
		if err != nil {
			return err
		}
		return task.Resize(ctx, width, height)
	}
	spec.Process.ConsoleSize = &specs.Box{Height: uint(height), Width: uint(width)}
	if err := e.store.UpdateSpec(ctx, container, spec); err != nil {
		return err
	}
	encoded, _ := json.Marshal(size)
	_, err = container.SetLabels(ctx, map[string]string{"io.porto.container.console-size": string(encoded)})
	return err
}
