package killswitch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
)

const (
	sourceName       = "porto"
	installScriptURL = "https://raw.githubusercontent.com/mbianchidev/kill-switch/012234dd7b65da030fbb9f88f4b5c283d0106de6/install.sh"
)

var (
	ErrBusy         = errors.New("KillSwitch integration is busy")
	ErrNotInstalled = errors.New("KillSwitch is not installed")
	ErrUnsupported  = errors.New("KillSwitch integration is supported only on macOS")
)

type CommandOutput struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	LookPath(name string) (string, error)
	Run(ctx context.Context, name string, args ...string) (CommandOutput, error)
}

type ExecRunner struct{}

func (ExecRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (CommandOutput, error) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return CommandOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

type Installer interface {
	Install(ctx context.Context) error
}

type ScriptInstaller struct {
	runner Runner
	url    string
}

func NewScriptInstaller(runner Runner) *ScriptInstaller {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &ScriptInstaller{runner: runner, url: installScriptURL}
}

func (i *ScriptInstaller) Install(ctx context.Context) error {
	curl, err := i.runner.LookPath("curl")
	if err != nil {
		return errors.New("curl is required to install KillSwitch")
	}
	bash, err := i.runner.LookPath("bash")
	if err != nil {
		return errors.New("bash is required to install KillSwitch")
	}
	env, err := i.runner.LookPath("env")
	if err != nil {
		return errors.New("env is required to install KillSwitch")
	}

	file, err := os.CreateTemp("", "porto-killswitch-install-*.sh")
	if err != nil {
		return fmt.Errorf("create KillSwitch installer file: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close KillSwitch installer file: %w", err)
	}
	defer os.Remove(path)

	output, err := i.runner.Run(
		ctx,
		curl,
		"--fail",
		"--location",
		"--silent",
		"--show-error",
		"--proto",
		"=https",
		"--tlsv1.2",
		"--max-time",
		"120",
		"--output",
		path,
		i.url,
	)
	if err != nil {
		return commandError("download KillSwitch installer", output, err)
	}
	output, err = i.runner.Run(ctx, env, "KILLSWITCH_INSTALL_MODE=release", bash, path)
	if err != nil {
		return commandError("install KillSwitch", output, err)
	}
	return nil
}

type Status struct {
	State           string    `json:"state"`
	Message         string    `json:"message"`
	UpdatedAt       time.Time `json:"updatedAt"`
	Supported       bool      `json:"supported"`
	Installed       bool      `json:"installed"`
	BinaryPath      string    `json:"binaryPath,omitempty"`
	Version         string    `json:"version,omitempty"`
	AutoKillEnabled *bool     `json:"autoKillEnabled"`
	UserPorts       []int     `json:"userPorts"`
	SyncedPorts     []int     `json:"syncedPorts"`
	EffectivePorts  []int     `json:"effectivePorts"`
}

type CLIStatus struct {
	Version          string           `json:"version"`
	AutoKillEnabled  *bool            `json:"autoKillEnabled"`
	UserPorts        []int            `json:"userPorts"`
	IntegrationPorts map[string][]int `json:"integrationPorts"`
	EffectivePorts   []int            `json:"effectivePorts"`
}

type KilledProcess struct {
	PID      int32   `json:"pid"`
	Command  string  `json:"command"`
	Runtime  string  `json:"runtime"`
	AgeHours float64 `json:"ageHours"`
}

type CleanupResult struct {
	Version         string          `json:"version"`
	AutoKillEnabled bool            `json:"autoKillEnabled"`
	CandidateCount  int             `json:"candidateCount"`
	KilledCount     int             `json:"killedCount"`
	KilledProcesses []KilledProcess `json:"killedProcesses"`
}

type Manager struct {
	runner    Runner
	installer Installer
	goos      string
	homeDir   func() (string, error)

	operationMu sync.Mutex

	statusMu sync.Mutex
	status   Status

	syncMu      sync.Mutex
	syncRunning bool
	syncPending bool
	pending     []int
}

func NewManager(runner Runner, installer Installer) *Manager {
	return newManager(runtime.GOOS, runner, installer, os.UserHomeDir)
}

func newManager(goos string, runner Runner, installer Installer, homeDir func() (string, error)) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	if installer == nil {
		installer = NewScriptInstaller(runner)
	}
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	now := time.Now().UTC()
	return &Manager{
		runner:    runner,
		installer: installer,
		goos:      goos,
		homeDir:   homeDir,
		status: Status{
			State:          "idle",
			Message:        "KillSwitch integration has not been checked.",
			UpdatedAt:      now,
			Supported:      goos == "darwin",
			UserPorts:      []int{},
			SyncedPorts:    []int{},
			EffectivePorts: []int{},
		},
	}
}

func (m *Manager) Snapshot() Status {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	return cloneStatus(m.status)
}

func (m *Manager) DisabledStatus() Status {
	status := m.Snapshot()
	status.State = "disabled"
	status.Message = "Integration is disabled."
	status.UpdatedAt = time.Now().UTC()
	status.Supported = m.goos == "darwin"
	if !status.Supported {
		status.Installed = false
		status.BinaryPath = ""
		return status
	}
	if binary, err := m.findBinary(); err == nil {
		status.Installed = true
		status.BinaryPath = binary
	} else {
		status.Installed = false
		status.BinaryPath = ""
	}
	return status
}

func (m *Manager) Probe(ctx context.Context) (Status, error) {
	if !m.operationMu.TryLock() {
		return m.Snapshot(), ErrBusy
	}
	defer m.operationMu.Unlock()
	m.setOperationStatus("checking", "Checking KillSwitch CLI.")
	return m.probe(ctx)
}

func (m *Manager) Sync(ctx context.Context, ports []int) (Status, error) {
	normalized, err := normalizePorts(ports)
	if err != nil {
		return m.setFailure(err), err
	}
	if !m.operationMu.TryLock() {
		return m.Snapshot(), ErrBusy
	}
	defer m.operationMu.Unlock()
	m.setOperationStatus("syncing", "Synchronizing active Porto ports with KillSwitch.")
	return m.sync(ctx, normalized)
}

func (m *Manager) RequestSync(ports []int, done func(Status, error)) error {
	normalized, err := normalizePorts(ports)
	if err != nil {
		status := m.setFailure(err)
		if done != nil {
			done(status, err)
		}
		return err
	}

	m.syncMu.Lock()
	m.pending = normalized
	m.syncPending = true
	if m.syncRunning {
		m.syncMu.Unlock()
		return nil
	}
	m.syncRunning = true
	m.syncMu.Unlock()

	go m.syncLoop(done)
	return nil
}

func (m *Manager) Install(ctx context.Context) (Status, error) {
	if !m.operationMu.TryLock() {
		return m.Snapshot(), ErrBusy
	}
	defer m.operationMu.Unlock()
	m.setOperationStatus("installing", "Installing KillSwitch.")
	return m.install(ctx)
}

func (m *Manager) StartInstall(done func(Status, error)) bool {
	if !m.operationMu.TryLock() {
		return false
	}
	m.setOperationStatus("installing", "Installing KillSwitch.")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		status, err := m.install(ctx)
		m.operationMu.Unlock()
		if done != nil {
			done(status, err)
		}
	}()
	return true
}

func (m *Manager) Cleanup(ctx context.Context) (CleanupResult, error) {
	if !m.operationMu.TryLock() {
		return CleanupResult{}, ErrBusy
	}
	defer m.operationMu.Unlock()
	m.setOperationStatus("cleaning", "Running KillSwitch dev cleanup.")

	if err := m.ensureSupported(); err != nil {
		m.setFailure(err)
		return CleanupResult{}, err
	}
	binary, err := m.findBinary()
	if err != nil {
		m.setFailure(err)
		return CleanupResult{}, err
	}

	var result CleanupResult
	if err := m.runJSON(ctx, 2*time.Minute, binary, &result, "dev-cleanup", "cleanup", "--json"); err != nil {
		m.setFailure(err)
		return CleanupResult{}, err
	}
	if result.KilledProcesses == nil {
		result.KilledProcesses = []KilledProcess{}
	}
	status := m.Snapshot()
	status.State = "ready"
	status.Message = fmt.Sprintf("KillSwitch cleanup found %d candidate(s) and killed %d.", result.CandidateCount, result.KilledCount)
	status.UpdatedAt = time.Now().UTC()
	status.Supported = true
	status.Installed = true
	status.BinaryPath = binary
	status.Version = result.Version
	status.AutoKillEnabled = boolPointer(result.AutoKillEnabled)
	m.setStatus(status)
	return result, nil
}

func (m *Manager) syncLoop(done func(Status, error)) {
	for {
		m.operationMu.Lock()

		m.syncMu.Lock()
		ports := append([]int(nil), m.pending...)
		m.syncPending = false
		m.syncMu.Unlock()

		m.setOperationStatus("syncing", "Synchronizing active Porto ports with KillSwitch.")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		status, err := m.sync(ctx, ports)
		cancel()
		m.operationMu.Unlock()

		if done != nil {
			done(status, err)
		}

		m.syncMu.Lock()
		if !m.syncPending {
			m.syncRunning = false
			m.syncMu.Unlock()
			return
		}
		m.syncMu.Unlock()
	}
}

func (m *Manager) probe(ctx context.Context) (Status, error) {
	if err := m.ensureSupported(); err != nil {
		return m.setFailure(err), err
	}
	binary, err := m.findBinary()
	if err != nil {
		return m.setFailure(err), err
	}
	cli, err := m.readCLIStatus(ctx, binary)
	if err != nil {
		return m.setFailure(err), err
	}
	status := m.readyStatus(binary, cli, "KillSwitch is ready.")
	m.setStatus(status)
	return status, nil
}

func (m *Manager) sync(ctx context.Context, ports []int) (Status, error) {
	if err := m.ensureSupported(); err != nil {
		return m.setFailure(err), err
	}
	binary, err := m.findBinary()
	if err != nil {
		return m.setFailure(err), err
	}

	var cli CLIStatus
	err = m.runJSON(
		ctx,
		15*time.Second,
		binary,
		&cli,
		"dev-cleanup",
		"sync-ports",
		"--source",
		sourceName,
		"--ports",
		joinPorts(ports),
		"--json",
	)
	if err != nil {
		return m.setFailure(err), err
	}

	message := fmt.Sprintf("Synchronized %d active Porto port(s) with KillSwitch.", len(ports))
	if len(ports) == 0 {
		message = "Cleared Porto-managed ports from KillSwitch."
	}
	status := m.readyStatus(binary, cli, message)
	m.setStatus(status)
	return status, nil
}

func (m *Manager) install(ctx context.Context) (Status, error) {
	if err := m.ensureSupported(); err != nil {
		return m.setFailure(err), err
	}
	if err := m.installer.Install(ctx); err != nil {
		return m.setFailure(err), err
	}
	binary, err := m.findBinary()
	if err != nil {
		err = fmt.Errorf("KillSwitch installer completed but killswitchctl is unavailable: %w", err)
		return m.setFailure(err), err
	}
	cli, err := m.readCLIStatus(ctx, binary)
	if err != nil {
		err = fmt.Errorf("KillSwitch installed but its CLI bridge is unavailable: %w", err)
		return m.setFailure(err), err
	}
	status := m.readyStatus(binary, cli, "KillSwitch installed and ready.")
	m.setStatus(status)
	return status, nil
}

func (m *Manager) readCLIStatus(ctx context.Context, binary string) (CLIStatus, error) {
	var cli CLIStatus
	err := m.runJSON(ctx, 5*time.Second, binary, &cli, "dev-cleanup", "status", "--json")
	return cli, err
}

func (m *Manager) runJSON(ctx context.Context, timeout time.Duration, binary string, target any, args ...string) error {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, binary, args...)
	if err != nil {
		if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("KillSwitch command timed out: %w", commandContext.Err())
		}
		return commandError("KillSwitch command", output, err)
	}
	stdout := bytes.TrimSpace(output.Stdout)
	if len(stdout) == 0 {
		return errors.New("KillSwitch command returned empty JSON output")
	}
	if err := json.Unmarshal(stdout, target); err != nil {
		return fmt.Errorf("decode KillSwitch JSON: %w", err)
	}
	return nil
}

func (m *Manager) ensureSupported() error {
	if m.goos != "darwin" {
		return ErrUnsupported
	}
	return nil
}

func (m *Manager) findBinary() (string, error) {
	if err := m.ensureSupported(); err != nil {
		return "", err
	}
	if binary, err := m.runner.LookPath("killswitchctl"); err == nil {
		return binary, nil
	}
	home, err := m.homeDir()
	if err != nil || home == "" {
		return "", ErrNotInstalled
	}
	binary := filepath.Join(home, "bin", "killswitchctl")
	info, err := os.Stat(binary)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", ErrNotInstalled
	}
	return binary, nil
}

func (m *Manager) readyStatus(binary string, cli CLIStatus, message string) Status {
	userPorts, _ := normalizePorts(cli.UserPorts)
	effectivePorts, _ := normalizePorts(cli.EffectivePorts)
	syncedPorts, _ := normalizePorts(cli.IntegrationPorts[sourceName])
	return Status{
		State:           "ready",
		Message:         message,
		UpdatedAt:       time.Now().UTC(),
		Supported:       true,
		Installed:       true,
		BinaryPath:      binary,
		Version:         cli.Version,
		AutoKillEnabled: cli.AutoKillEnabled,
		UserPorts:       userPorts,
		SyncedPorts:     syncedPorts,
		EffectivePorts:  effectivePorts,
	}
}

func (m *Manager) setOperationStatus(state, message string) {
	status := m.Snapshot()
	status.State = state
	status.Message = message
	status.UpdatedAt = time.Now().UTC()
	status.Supported = m.goos == "darwin"
	m.setStatus(status)
}

func (m *Manager) setFailure(err error) Status {
	status := m.Snapshot()
	status.State = "error"
	status.Message = err.Error()
	status.UpdatedAt = time.Now().UTC()
	status.Supported = m.goos == "darwin"
	if errors.Is(err, ErrUnsupported) {
		status.State = "unsupported"
		status.Installed = false
		status.BinaryPath = ""
	}
	if errors.Is(err, ErrNotInstalled) {
		status.State = "missing"
		status.Installed = false
		status.BinaryPath = ""
		status.Message = "KillSwitch is not installed. Install it to enable port sync and cleanup."
	}
	m.setStatus(status)
	return status
}

func (m *Manager) setStatus(status Status) {
	m.statusMu.Lock()
	m.status = cloneStatus(status)
	m.statusMu.Unlock()
}

func ManagedPorts(projects []app.Project) []int {
	reserved := map[int]bool{}
	for _, address := range []string{config.DaemonAddr, config.RouterAddr} {
		_, rawPort, err := net.SplitHostPort(address)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(rawPort)
		if err == nil {
			reserved[port] = true
		}
	}

	ports := make([]int, 0, len(projects))
	seen := map[int]bool{}
	for _, project := range projects {
		if project.Status != "running" || project.PID <= 0 || project.Port <= 0 || reserved[project.Port] || seen[project.Port] {
			continue
		}
		seen[project.Port] = true
		ports = append(ports, project.Port)
	}
	sort.Ints(ports)
	return ports
}

func normalizePorts(ports []int) ([]int, error) {
	normalized := make([]int, 0, len(ports))
	seen := map[int]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port %d", port)
		}
		if seen[port] {
			continue
		}
		seen[port] = true
		normalized = append(normalized, port)
	}
	sort.Ints(normalized)
	return normalized, nil
}

func joinPorts(ports []int) string {
	values := make([]string, len(ports))
	for i, port := range ports {
		values[i] = strconv.Itoa(port)
	}
	return strings.Join(values, ",")
}

func commandError(action string, output CommandOutput, err error) error {
	message := strings.TrimSpace(string(output.Stderr))
	if message == "" {
		message = strings.TrimSpace(string(output.Stdout))
	}
	if message == "" {
		return fmt.Errorf("%s failed: %w", action, err)
	}
	return fmt.Errorf("%s failed: %w: %s", action, err, message)
}

func cloneStatus(status Status) Status {
	status.UserPorts = append([]int(nil), status.UserPorts...)
	status.SyncedPorts = append([]int(nil), status.SyncedPorts...)
	status.EffectivePorts = append([]int(nil), status.EffectivePorts...)
	if status.AutoKillEnabled != nil {
		status.AutoKillEnabled = boolPointer(*status.AutoKillEnabled)
	}
	return status
}

func boolPointer(value bool) *bool {
	return &value
}
