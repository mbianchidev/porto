package etcqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type Manager struct {
	mu         sync.Mutex
	root       string
	enabled    bool
	supported  bool
	runner     runtimes.Runner
	lookPath   func(string) (string, error)
	newCommand func(context.Context, string, string, ...string) *exec.Cmd
	identify   func(int) (*ProcessIdentity, error)
	terminate  func(int) error
	kill       func(int) error
	token      func() string
	guard      func(context.Context, string, string) error
}

func New(root string, runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{
		root: root, supported: Supported(),
		runner: runner, lookPath: runtimes.LookPath, newCommand: process.NewCommand,
		identify: inspectPID, terminate: terminatePID, kill: killPID,
		token: func() string { return os.Getenv("PORTO_ETCQLITE_TOKEN") },
	}
}

func Supported() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }

// SetEnabled and SetGuard are initialization-only; transitions use Configure.
func (m *Manager) SetEnabled(enabled bool) { m.enabled = enabled }

func (m *Manager) SetGuard(guard func(context.Context, string, string) error) { m.guard = guard }

func (m *Manager) Configure(ctx context.Context, enabled bool, persist func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if enabled && !m.supported {
		return ErrUnsupported
	}
	if m.enabled && !enabled {
		names, err := m.names()
		if err != nil {
			return err
		}
		for _, name := range names {
			instance, err := m.loadInstance(name)
			if err != nil {
				return err
			}
			if instance.DesiredRunning {
				return fmt.Errorf("%w: stop Etcqlite %s explicitly before disabling", ErrConflict, name)
			}
			if m.guard != nil {
				if err := m.guard(ctx, name, instance.ClusterID); err != nil {
					return err
				}
			}
			for _, member := range instance.Members {
				if alive, err := m.alive(member.Process); err != nil {
					return err
				} else if alive {
					return fmt.Errorf("%w: Etcqlite %s still has a running member", ErrConflict, name)
				}
				if err := listenersAvailable(member); err != nil {
					return err
				}
			}
		}
	}
	if err := persist(); err != nil {
		return err
	}
	m.enabled = enabled
	return nil
}

func (m *Manager) lock(ctx context.Context) (func(), error) {
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()
		return nil, ErrDisabled
	}
	if !m.supported {
		m.mu.Unlock()
		return nil, ErrUnsupported
	}
	if m.root == "" {
		m.mu.Unlock()
		return nil, errors.New("Etcqlite state root is unavailable")
	}
	if !filepath.IsAbs(m.root) {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: Etcqlite requires an absolute node-local PORTO_HOME", ErrInvalid)
	}
	if err := privateDirectory(m.root); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if err := requireLocalStorage(m.root); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	lease, err := filelock.Acquire(ctx, filepath.Join(m.root, "lifecycle.lock"))
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	return func() {
		if err := lease.Close(); err != nil {
			slog.Error("Close Etcqlite lifecycle lock", "error", err)
		}
		m.mu.Unlock()
	}, nil
}

func (m *Manager) binaries() (string, string, error) {
	resolve := func(variable, name string) (string, error) {
		if path := os.Getenv(variable); path != "" {
			if !filepath.IsAbs(path) {
				return "", fmt.Errorf("%s must be an absolute executable path", variable)
			}
			return exec.LookPath(path)
		}
		return m.lookPath(name)
	}
	binary, err := resolve("PORTO_ETCQLITE_BINARY", "etcqlite")
	if err != nil {
		return "", "", fmt.Errorf("%w: install matching etcqlite and etcqlite-snapshot binaries separately and restart Porto: %w", ErrUnavailable, err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve native Etcqlite executable: %w", ErrUnavailable, err)
	}
	candidate := filepath.Join(filepath.Dir(binary), "etcqlite-snapshot")
	helper, err := exec.LookPath(candidate)
	if err != nil {
		return "", "", fmt.Errorf("%w: install the matching native snapshot helper alongside the resolved etcqlite binary: %w", ErrUnavailable, err)
	}
	helper, err = filepath.EvalSymlinks(helper)
	if err != nil {
		return "", "", err
	}
	if override := os.Getenv("PORTO_ETCQLITE_SNAPSHOT_BINARY"); override != "" {
		configured, err := filepath.EvalSymlinks(override)
		if err != nil || configured != helper {
			return "", "", errors.Join(ErrInvalid, errors.New("PORTO_ETCQLITE_SNAPSHOT_BINARY must resolve to the primary binary's matching sibling helper"), err)
		}
	}
	return binary, helper, nil
}

func (m *Manager) Create(ctx context.Context, request CreateRequest) (instance Instance, err error) {
	release, err := m.lock(ctx)
	if err != nil {
		return instance, err
	}
	defer release()
	request, err = validateCreate(request)
	if err != nil {
		return instance, err
	}
	binary, _, err := m.binaries()
	if err != nil {
		return instance, err
	}
	instances := filepath.Join(m.root, "instances")
	if err := privateDirectory(instances); err != nil {
		return instance, err
	}
	instance = Instance{Format: 1, Name: request.Name, State: "initializing", CreatedAt: time.Now().UTC()}
	instance.ClusterID, err = randomID()
	if err != nil {
		return instance, err
	}
	instance.Members, err = allocateMembers(request)
	if err != nil {
		return instance, err
	}
	directory := m.instancePath(request.Name)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return instance, fmt.Errorf("%w: create new Etcqlite instance without reusing existing state: %v", ErrConflict, err)
	}
	if err := m.writeInstance(instance); err != nil {
		return instance, err
	}
	defer func() {
		if err != nil {
			instance.State, instance.Message = "error", err.Error()
			err = errors.Join(err, m.writeInstance(instance))
		}
	}()
	if err = m.createCertificates(instance); err != nil {
		return instance, fmt.Errorf("create Etcqlite mTLS identities: %w", err)
	}
	clusterID, err := strconv.ParseUint(instance.ClusterID, 10, 64)
	if err != nil {
		return instance, err
	}
	config := genesis{ClusterID: clusterID, ConfigurationID: 1}
	for index, member := range instance.Members {
		id, err := strconv.ParseUint(member.ID, 10, 64)
		if err != nil {
			return instance, err
		}
		config.Members = append(config.Members, genesisMember{
			ID: id, PID: uint64(index + 1), Name: member.Name, ClientURL: member.ClientURL, PeerURL: member.PeerURL,
		})
	}
	configPath := filepath.Join(directory, "genesis.json")
	data, err := json.Marshal(config)
	if err != nil {
		return instance, err
	}
	if err = writeExclusive(configPath, data); err != nil {
		return instance, err
	}
	for index := range instance.Members {
		member := &instance.Members[index]
		memberDirectory := m.memberPath(instance, *member)
		if err = privateDirectory(filepath.Join(directory, "members")); err != nil {
			return instance, err
		}
		if err = privateDirectory(memberDirectory); err != nil {
			return instance, err
		}
		args := []string{"init", "--data-dir", filepath.Join(memberDirectory, "data"), "--member-id", member.ID}
		if len(instance.Members) == 1 {
			args = append(args, "--cluster-id", instance.ClusterID, "--name", member.Name)
		} else {
			args = append(args, "--cluster-config", configPath)
		}
		output, runErr := m.runner.Run(ctx, runtimes.Command{Name: binary, Args: args})
		if runErr != nil {
			return instance, runtimes.CommandError("initialize Etcqlite "+member.Name, output, runErr)
		}
		if err := m.checkNode(instance, *member); err != nil {
			return instance, err
		}
		member.Initialized = true
		if err = m.writeInstance(instance); err != nil {
			return instance, err
		}
	}
	instance.State, instance.Message = "stopped", "Initialized private node-local data and mTLS identities. Start explicitly."
	return instance, m.writeInstance(instance)
}

func (m *Manager) expected(name, clusterID string) (Instance, error) {
	instance, err := m.loadInstance(name)
	if err != nil {
		return instance, err
	}
	if clusterID == "" || instance.ClusterID != clusterID {
		return instance, fmt.Errorf("%w: Etcqlite cluster identity changed; refresh the selected resource", ErrConflict)
	}
	return instance, nil
}

func (m *Manager) Start(ctx context.Context, name, clusterID string) (Instance, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return Instance{}, err
	}
	defer release()
	instance, err := m.expected(name, clusterID)
	if err != nil {
		return instance, err
	}
	return m.start(ctx, instance)
}

func (m *Manager) start(ctx context.Context, instance Instance) (result Instance, err error) {
	binary, helper, err := m.binaries()
	if err != nil {
		return instance, err
	}
	for _, member := range instance.Members {
		if !member.Initialized {
			return instance, errors.New("Etcqlite initialization was interrupted; existing data is retained and will not be reinitialized")
		}
		if err := m.checkNode(instance, member); err != nil {
			return instance, err
		}
	}
	instance.DesiredRunning = true
	instance.State, instance.Message = "starting", "Waiting for authenticated identity and quorum/read readiness."
	if err := m.writeInstance(instance); err != nil {
		return instance, err
	}
	var started []*ProcessIdentity
	defer func() {
		if err != nil {
			for _, identity := range started {
				cleanupContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err = errors.Join(err, m.stopIdentity(cleanupContext, identity))
				cancel()
			}
			instance.State, instance.Message = "error", err.Error()
			err = errors.Join(err, m.writeInstance(instance))
			result = instance
		}
	}()
	for index := range instance.Members {
		member := &instance.Members[index]
		alive, inspectErr := m.alive(member.Process)
		if inspectErr != nil {
			return instance, inspectErr
		}
		if alive {
			continue
		}
		if err := listenersAvailable(*member); err != nil {
			return instance, err
		}
		identity, launchErr := m.launch(instance, *member, binary, helper)
		if launchErr != nil {
			return instance, launchErr
		}
		member.Process = identity
		started = append(started, identity)
		if err := m.writeInstance(instance); err != nil {
			return instance, err
		}
	}
	for {
		var failures []error
		for _, member := range instance.Members {
			alive, inspectErr := m.alive(member.Process)
			if inspectErr != nil {
				return instance, inspectErr
			}
			if !alive {
				return instance, fmt.Errorf("Etcqlite %s exited before readiness; inspect its stdout/stderr logs", member.Name)
			}
			if _, probeErr := m.probe(ctx, instance, member); probeErr != nil {
				failures = append(failures, fmt.Errorf("%s: %w", member.Name, probeErr))
			}
		}
		if len(failures) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return instance, fmt.Errorf("Etcqlite did not become ready: %w", errors.Join(ctx.Err(), errors.Join(failures...)))
		case <-time.After(250 * time.Millisecond):
		}
	}
	instance.State, instance.Message = "ready", "All members passed authenticated identity and quorum/read readiness."
	return instance, m.writeInstance(instance)
}

func (m *Manager) launch(instance Instance, member Member, binary, helper string) (*ProcessIdentity, error) {
	tlsDirectory := filepath.Join(m.instancePath(instance.Name), "tls")
	args := []string{"serve", "--data-dir", filepath.Join(m.memberPath(instance, member), "data"),
		"--listen", member.Listen, "--cert-file", filepath.Join(tlsDirectory, member.Name+".pem"),
		"--key-file", filepath.Join(tlsDirectory, member.Name+"-key.pem"),
		"--client-ca-file", filepath.Join(tlsDirectory, "ca.pem"), "--snapshot-tool", helper}
	if len(instance.Members) > 1 {
		args = append(args, "--peer-listen", member.PeerListen,
			"--peer-cert-file", filepath.Join(tlsDirectory, member.Name+".pem"),
			"--peer-key-file", filepath.Join(tlsDirectory, member.Name+"-key.pem"),
			"--peer-ca-file", filepath.Join(tlsDirectory, "ca.pem"))
	}
	stdout, err := os.OpenFile(filepath.Join(m.memberPath(instance, member), "stdout.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(m.memberPath(instance, member), "stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	// Native members outlive the daemon. Direct file stdio avoids broken pipes on restart.
	cmd := m.newCommand(context.Background(), m.memberPath(instance, member), binary, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start native Etcqlite member: %w", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := cmd.Wait()
		closeErr := errors.Join(stdout.Close(), stderr.Close())
		if err != nil || closeErr != nil {
			slog.Warn("Etcqlite member exited", "instance", instance.Name, "member", member.Name, "error", err, "logError", closeErr)
		}
	}()
	identity, err := m.identify(cmd.Process.Pid)
	if err != nil {
		terminateErr := process.Kill(cmd)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			terminateErr = errors.Join(terminateErr, errors.New("unidentified Etcqlite child did not exit after termination"))
		}
		return nil, errors.Join(fmt.Errorf("record native Etcqlite process identity: %w", err), terminateErr)
	}
	return identity, nil
}

func listenersAvailable(member Member) error {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, address := range []string{member.Listen, member.PeerListen} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("%w: %s listener %s is occupied; no existing process will be stopped or replaced: %v", ErrConflict, member.Name, address, err)
		}
		listeners = append(listeners, listener)
	}
	return nil
}

func (m *Manager) alive(expected *ProcessIdentity) (bool, error) {
	if expected == nil {
		return false, nil
	}
	current, err := m.identify(expected.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("verify Etcqlite process ownership without signaling it: %w", err)
	}
	return current.PID == expected.PID && current.Birth == expected.Birth && current.Executable == expected.Executable, nil
}

func (m *Manager) stopIdentity(ctx context.Context, identity *ProcessIdentity) error {
	alive, err := m.alive(identity)
	if err != nil || !alive {
		return err
	}
	if err := m.terminate(identity.PID); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("gracefully stop verified Etcqlite process: %w", err)
	}
	started := time.Now()
	var inspectionErr error
	for {
		alive, err := m.alive(identity)
		if err == nil && !alive {
			return nil
		}
		if err != nil {
			inspectionErr = err
			slog.Debug("Etcqlite exit ownership is not yet observable; no additional signal sent", "pid", identity.PID, "error", err)
		}
		if err == nil && time.Since(started) >= 10*time.Second {
			if err := m.kill(identity.PID); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("force-stop verified Etcqlite process: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("Etcqlite process did not stop with verified ownership: %w", errors.Join(ctx.Err(), inspectionErr))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (m *Manager) Stop(ctx context.Context, name, clusterID string) (Instance, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return Instance{}, err
	}
	defer release()
	instance, err := m.expected(name, clusterID)
	if err != nil {
		return instance, err
	}
	if m.guard != nil {
		if err := m.guard(ctx, name, clusterID); err != nil {
			return instance, err
		}
	}
	return m.stop(ctx, instance)
}

func (m *Manager) stop(ctx context.Context, instance Instance) (Instance, error) {
	instance.DesiredRunning = false
	if err := m.writeInstance(instance); err != nil {
		return instance, err
	}
	var failures []error
	for index := range instance.Members {
		member := &instance.Members[index]
		if err := m.stopIdentity(ctx, member.Process); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", member.Name, err))
			continue
		}
		if err := listenersAvailable(*member); err != nil {
			failures = append(failures, err)
			continue
		}
		member.Process = nil
		if err := m.writeInstance(instance); err != nil {
			failures = append(failures, err)
		}
	}
	if err := errors.Join(failures...); err != nil {
		instance.State, instance.Message = "error", err.Error()
		return instance, errors.Join(err, m.writeInstance(instance))
	}
	instance.State, instance.Message = "stopped", "Native members stopped. Identity, certificates, logs and node-local data are retained."
	return instance, m.writeInstance(instance)
}

func (m *Manager) Reconcile(ctx context.Context) error {
	release, err := m.lock(ctx)
	if errors.Is(err, ErrDisabled) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	names, err := m.names()
	if err != nil {
		return err
	}
	var failures []error
	for _, name := range names {
		instance, err := m.loadInstance(name)
		if err == nil && instance.DesiredRunning {
			memberContext, cancel := context.WithTimeout(ctx, OperationTimeout)
			_, err = m.start(memberContext, instance)
			cancel()
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile Etcqlite %s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) Status(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := Status{Supported: m.supported, State: "disabled", Message: "Disabled. No binary lookup, installation, scanning, startup or cluster retargeting.", Instances: []Instance{}}
	if !m.enabled {
		return status, nil
	}
	if !m.supported {
		status.State, status.Message = "unsupported", "Managed Etcqlite native lifecycle currently supports macOS and Linux only."
		return status, nil
	}
	status.State, status.Message = "idle", "Create and start Etcqlite explicitly. Existing Kubernetes datastore defaults are unchanged."
	names, err := m.names()
	if err != nil {
		return status, err
	}
	for _, name := range names {
		instance, err := m.loadInstance(name)
		if err != nil {
			return status, err
		}
		ready, running, failed := 0, 0, false
		for index := range instance.Members {
			member := &instance.Members[index]
			alive, err := m.alive(member.Process)
			if err != nil {
				member.State, member.Message = "error", err.Error()
				failed = true
				continue
			}
			if !alive {
				member.State = "stopped"
				if !member.Initialized {
					member.State, member.Message = "error", "Initialization is incomplete; existing data will not be reinitialized."
					failed = true
				}
				continue
			}
			if err := m.checkNode(instance, *member); err != nil {
				member.State, member.Message, failed = "error", err.Error(), true
				continue
			}
			running++
			if _, err := m.probe(ctx, instance, *member); err != nil {
				member.State, member.Message = "error", err.Error()
			} else {
				member.State, member.Message = "ready", "Authenticated identity and quorum/read readiness passed."
				ready++
			}
			if usage, err := process.ResourceStats(ctx, member.Process.PID, true); err == nil {
				member.Usage = &usage
			} else {
				member.Message += " Resource usage unavailable: " + err.Error()
			}
		}
		switch {
		case ready == len(instance.Members):
			instance.State, instance.Message = "ready", "All members passed authenticated identity and quorum/read readiness."
		case ready >= len(instance.Members)/2+1:
			instance.State, instance.Message = "degraded", "Quorum is readable, but not all owned members are ready. Inspect member failures."
		case running > 0 || instance.DesiredRunning || instance.State == "error" || failed:
			instance.State, instance.Message = "error", "Owned members are not ready. Inspect member errors and retained stdout/stderr logs."
		default:
			instance.State, instance.Message = "stopped", "Members are stopped; identity and node-local data are retained."
		}
		status.Instances = append(status.Instances, instance)
	}
	if len(status.Instances) > 0 {
		status.State, status.Message = "configured", "Explicitly owned local members. Three members on one host are not independent failure domains."
	}
	return status, nil
}

func (m *Manager) Connection(ctx context.Context, name, clusterID string) (Connection, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer release()
	instance, err := m.loadInstance(name)
	if err != nil {
		return Connection{}, err
	}
	if clusterID != "" && instance.ClusterID != clusterID {
		return Connection{}, fmt.Errorf("%w: selected Etcqlite datastore identity changed", ErrConflict)
	}
	ready := 0
	var failures []error
	for _, member := range instance.Members {
		if err := m.checkNode(instance, member); err != nil {
			return Connection{}, err
		}
		endpoint, err := url.Parse(member.ClientURL)
		if err != nil {
			return Connection{}, err
		}
		host, _, err := net.SplitHostPort(member.Listen)
		if err != nil || loopbackHost(endpoint.Hostname()) || net.ParseIP(host).IsLoopback() {
			return Connection{}, errors.New("k3s VM datastore needs an explicitly guest-reachable Etcqlite advertisement and listener; loopback endpoints are not retargeted")
		}
		if _, err := m.probe(ctx, instance, member); err != nil {
			failures = append(failures, err)
		} else {
			ready++
		}
	}
	if ready < len(instance.Members)/2+1 {
		return Connection{}, fmt.Errorf("selected Etcqlite datastore lacks verified quorum/read readiness; start or repair it explicitly: %w", errors.Join(failures...))
	}
	return m.tlsMaterial(instance)
}

func (m *Manager) Logs(ctx context.Context, name, stream string, tail int) ([]string, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	instance, err := m.loadInstance(name)
	if err != nil {
		return nil, err
	}
	if (stream != "stdout" && stream != "stderr") || tail < 1 || tail > 1000 {
		return nil, fmt.Errorf("%w: logs require stream stdout|stderr and tail between 1 and 1000", ErrInvalid)
	}
	lines := []string{}
	for _, member := range instance.Members {
		path := filepath.Join(m.memberPath(instance, member), stream+".log")
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		offset := max(int64(0), info.Size()-(1<<20))
		reader := io.NewSectionReader(file, offset, info.Size()-offset)
		first := true
		err = process.Stream(reader, func(line string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if offset == 0 || !first {
				lines = append(lines, member.Name+": "+line)
			}
			first = false
			if len(lines) > tail {
				lines = lines[len(lines)-tail:]
			}
			return nil
		})
		err = errors.Join(err, file.Close())
		if err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func (m *Manager) Backups(ctx context.Context) ([]Backup, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	entries, err := os.ReadDir(filepath.Join(m.root, "backups"))
	if errors.Is(err, os.ErrNotExist) {
		return []Backup{}, nil
	}
	if err != nil {
		return nil, err
	}
	backups := []Backup{}
	for _, entry := range entries {
		if !entry.IsDir() || !namePattern.MatchString(entry.Name()) {
			return nil, errors.New("invalid entry in Etcqlite backup metadata")
		}
		data, err := os.ReadFile(filepath.Join(m.root, "backups", entry.Name(), "verified.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var backup Backup
		if err := json.Unmarshal(data, &backup); err != nil {
			return nil, err
		}
		backups = append(backups, backup)
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].CreatedAt.After(backups[j].CreatedAt) })
	return backups, nil
}

func (m *Manager) runSnapshotStatus(ctx context.Context, binary, path, clusterID, memberID string) (SnapshotStatus, error) {
	var status SnapshotStatus
	output, err := m.runner.Run(ctx, runtimes.Command{
		Name: binary, Args: []string{"snapshot", "status", "--input", path}, StdoutOnly: true,
	})
	if err != nil {
		return status, runtimes.CommandError("verify native Etcqlite snapshot integrity", output, err)
	}
	if err := json.Unmarshal(output, &status); err != nil {
		return status, fmt.Errorf("decode native snapshot verification: %w", err)
	}
	return status, verifySnapshot(path, status, clusterID, memberID)
}

func (m *Manager) Backup(ctx context.Context, name, clusterID string) (Backup, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return Backup{}, err
	}
	defer release()
	instance, err := m.expected(name, clusterID)
	if err != nil {
		return Backup{}, err
	}
	return m.backup(ctx, &instance)
}

func (m *Manager) backup(ctx context.Context, instance *Instance) (Backup, error) {
	binary, _, err := m.binaries()
	if err != nil {
		return Backup{}, err
	}
	id, err := randomID()
	if err != nil {
		return Backup{}, err
	}
	root := filepath.Join(m.root, "backups")
	if err := privateDirectory(root); err != nil {
		return Backup{}, err
	}
	directory := filepath.Join(root, "backup-"+id)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return Backup{}, err
	}
	backup := Backup{ID: "backup-" + id, Path: directory, Name: instance.Name, ClusterID: instance.ClusterID, CreatedAt: time.Now().UTC()}
	for _, member := range instance.Members {
		if err := m.checkNode(*instance, member); err != nil {
			return Backup{}, err
		}
		path := filepath.Join(directory, member.Name+".snap")
		alive, err := m.alive(member.Process)
		if err != nil {
			return Backup{}, err
		}
		var discovery Discovery
		if alive {
			client, base, err := m.client(*instance, member)
			if err != nil {
				return Backup{}, err
			}
			discovery, err = nativeDiscovery(ctx, client, base, m.token(), instance.ClusterID, member.ID)
			if err != nil {
				return Backup{}, err
			}
			if err := nativeReady(ctx, client, base, m.token()); err != nil {
				return Backup{}, err
			}
			if err := nativeSnapshot(ctx, client, base, m.token(), discovery, path); err != nil {
				return Backup{}, err
			}
		} else {
			if err := listenersAvailable(member); err != nil {
				return Backup{}, err
			}
			output, err := m.runner.Run(ctx, runtimes.Command{
				Name: binary, Args: []string{"snapshot", "save", "--data-dir", filepath.Join(m.memberPath(*instance, member), "data"), "--output", path},
			})
			if err != nil {
				return Backup{}, runtimes.CommandError("save stopped member's native Etcqlite snapshot", output, err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return Backup{}, err
			}
		}
		status, err := m.runSnapshotStatus(ctx, binary, path, instance.ClusterID, member.ID)
		if err != nil {
			return Backup{}, err
		}
		if alive && (status.SchemaVersion != discovery.SchemaVersion || status.Revision < discovery.Revision) {
			return Backup{}, errors.New("native recovery image does not cover the discovered member schema/revision")
		}
		backup.Snapshots = append(backup.Snapshots, Snapshot{Member: member.Name, Path: path, Status: status})
	}
	data, err := json.Marshal(backup)
	if err != nil {
		return Backup{}, err
	}
	if err := writeExclusive(filepath.Join(directory, "verified.json"), data); err != nil {
		return Backup{}, err
	}
	instance.BackupID = backup.ID
	if err := m.writeInstance(*instance); err != nil {
		return Backup{}, err
	}
	return backup, nil
}

func (m *Manager) Delete(ctx context.Context, name, clusterID string, confirm bool) (string, error) {
	if !confirm {
		return "", fmt.Errorf("%w: confirm=true is required to back up, stop and retain an instance", ErrInvalid)
	}
	release, err := m.lock(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	instance, err := m.expected(name, clusterID)
	if err != nil {
		return "", err
	}
	if m.guard != nil {
		if err := m.guard(ctx, name, clusterID); err != nil {
			return "", err
		}
	}
	if _, err := m.backup(ctx, &instance); err != nil {
		return "", fmt.Errorf("native backup verification failed; no member was stopped or data removed: %w", err)
	}
	instance, err = m.stop(ctx, instance)
	if err != nil {
		return "", err
	}
	closeLocks, err := lockNodeDirectories(m.instancePath(name), instance.Members)
	if err != nil {
		return "", fmt.Errorf("Etcqlite data is still writer-owned; retention was refused: %w", err)
	}
	defer closeLocks()
	retained := filepath.Join(m.root, "retained")
	if err := privateDirectory(retained); err != nil {
		return "", err
	}
	target := m.retainedPath(name, clusterID)
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: retained Etcqlite identity already exists or cannot be inspected", ErrConflict)
	}
	if err := os.Rename(m.instancePath(name), target); err != nil {
		return "", fmt.Errorf("retain Etcqlite data for reversible deletion: %w", err)
	}
	return target, nil
}

func (m *Manager) Restore(ctx context.Context, name, clusterID string) (Instance, error) {
	release, err := m.lock(ctx)
	if err != nil {
		return Instance{}, err
	}
	defer release()
	if _, err := identityETag(clusterID, "1"); err != nil || !namePattern.MatchString(name) {
		return Instance{}, fmt.Errorf("%w: restore requires the original name and clusterID", ErrInvalid)
	}
	source := m.retainedPath(name, clusterID)
	instance, err := loadInstanceDirectory(source, name)
	if err != nil {
		return Instance{}, err
	}
	if instance.ClusterID != clusterID || instance.DesiredRunning {
		return Instance{}, errors.New("retained Etcqlite identity or lifecycle state changed")
	}
	if _, err := os.Lstat(m.instancePath(name)); !errors.Is(err, os.ErrNotExist) {
		return Instance{}, fmt.Errorf("%w: restoration never overwrites an existing instance", ErrConflict)
	}
	for _, member := range instance.Members {
		if err := listenersAvailable(member); err != nil {
			return Instance{}, err
		}
	}
	closeLocks, err := lockNodeDirectories(source, instance.Members)
	if err != nil {
		return Instance{}, fmt.Errorf("retained Etcqlite data has a writer; restoration was refused: %w", err)
	}
	defer closeLocks()
	if err := os.Rename(source, m.instancePath(name)); err != nil {
		return Instance{}, err
	}
	instance.State, instance.Message = "stopped", "Retained identity and node-local data restored without startup or datastore retargeting."
	return instance, m.writeInstance(instance)
}

func lockNodeDirectories(directory string, members []Member) (func(), error) {
	var locks []*filelock.Lock
	closeLocks := func() {
		for _, lease := range locks {
			if err := lease.Close(); err != nil {
				slog.Error("Release existing Etcqlite node writer lock", "error", err)
			}
		}
	}
	for _, member := range members {
		path := filepath.Join(directory, "members", member.Name, "data", "node.lock")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			closeLocks()
			return nil, errors.Join(errors.New("Etcqlite node.lock must already exist as a private regular file"), err)
		}
		lease, err := filelock.TryAcquireExisting(path)
		if err != nil {
			closeLocks()
			return nil, fmt.Errorf("lock existing Etcqlite %s writer inode without changing it: %w", member.Name, err)
		}
		locks = append(locks, lease)
	}
	return closeLocks, nil
}
