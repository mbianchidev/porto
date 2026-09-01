package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const (
	defaultTimeout     = 20 * time.Second
	engineInstanceName = "porto-engine"
	engineStateFile    = "engine.json"
	engineLockFile     = "engine-install.lock"
)

var (
	ErrUnavailable = errors.New("Porto container runtime is unavailable")
	ErrUnsupported = errors.New("Docker operation is not supported by Porto")
)

type Manager struct {
	runner       runtimes.Runner
	timeout      time.Duration
	stateDir     string
	lookPath     func(string) (string, error)
	goos         string
	directCLI    bool
	dialBuildKit func(context.Context) (net.Conn, error)
	installMu    sync.Mutex
	healthMu     sync.Mutex
}

type engineState struct {
	Mode      string    `json:"mode"`
	Instance  string    `json:"instance,omitempty"`
	OwnerID   string    `json:"ownerId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func New(runner runtimes.Runner) *Manager {
	if runner != nil {
		return &Manager{
			runner:    runner,
			timeout:   defaultTimeout,
			lookPath:  exec.LookPath,
			goos:      runtime.GOOS,
			directCLI: true,
		}
	}
	stateDir, _ := config.DockerEngineDir()
	return NewWithStateDir(nil, stateDir)
}

func NewWithStateDir(runner runtimes.Runner, stateDir string) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{
		runner:   runner,
		timeout:  defaultTimeout,
		stateDir: stateDir,
		lookPath: exec.LookPath,
		goos:     runtime.GOOS,
	}
}

func (m *Manager) Status(ctx context.Context, socketPath string) Status {
	status := Status{
		Context:       "porto",
		Endpoint:      dockerEndpoint(socketPath),
		ClientVersion: config.Version,
		ServerVersion: config.Version,
		ProxySocket:   socketPath,
	}
	backend, err := m.backend(ctx)
	if err != nil {
		status.Message = err.Error()
		return status
	}
	output, err := m.runBackend(ctx, backend, 10*time.Second, "inspect Porto container runtime", nil, "version")
	if err != nil {
		status.Message = err.Error()
		return status
	}
	status.Available = true
	status.Backend = backend.description
	if version := firstVersionLine(string(output)); version != "" {
		status.ServerVersion = version
	}
	return status
}

func (m *Manager) InstallEngine(ctx context.Context) (status Status, err error) {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	lock, err := acquireEngineInstallLock(filepath.Join(m.stateDir, engineLockFile))
	if err != nil {
		return Status{}, fmt.Errorf("lock Porto container runtime installation: %w", err)
	}
	defer func() {
		err = errors.Join(err, lock.Close())
	}()

	existingState, stateErr := m.readEngineState()
	ownedLima := stateErr == nil && existingState.Mode == "lima" && existingState.Instance == engineInstanceName && existingState.OwnerID != ""
	if !ownedLima {
		if path, err := m.lookPath("nerdctl"); err == nil {
			direct := commandBackend{name: path, description: "containerd via nerdctl"}
			if output, verifyErr := m.runBackend(ctx, direct, 10*time.Second, "verify local containerd", nil, "version"); verifyErr == nil {
				buildKit, buildKitErr := m.dialBuildKitBackend(ctx, direct)
				if buildKitErr == nil {
					_ = buildKit.Close()
					if err := m.writeEngineState(engineState{Mode: "direct", CreatedAt: time.Now().UTC()}); err != nil {
						return Status{}, err
					}
					return installedStatus(direct, output), nil
				}
			}
		}
	}
	if m.goos == "windows" {
		return Status{}, errors.New("Porto engine installation is not available on Windows; install nerdctl and containerd manually")
	}
	if _, err := m.lookPath("limactl"); err != nil {
		return Status{}, errors.New("Lima is required to install the Porto container runtime; install limactl and retry")
	}

	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return Status{}, err
	}
	if exists && !ownedLima {
		return Status{}, fmt.Errorf("Lima instance %q already exists but is not owned by Porto", engineInstanceName)
	}
	created := false
	ownerID := existingState.OwnerID
	if !exists {
		created = true
		ownerID, err = randomResourceName()
		if err != nil {
			return Status{}, err
		}
		_, err = m.runCommand(
			ctx,
			10*time.Minute,
			"create Porto container runtime",
			nil,
			"limactl",
			"start",
			"--tty=false",
			"--name="+engineInstanceName,
			"--containerd=user",
			"--mount-writable",
			"template://default",
		)
	} else if !running {
		_, err = m.runCommand(ctx, 5*time.Minute, "start Porto container runtime", nil, "limactl", "start", engineInstanceName)
	}
	if err != nil {
		if created {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, cleanupErr := m.runCommand(cleanupContext, 5*time.Minute, "clean up failed Porto container runtime", nil, "limactl", "delete", "--force", engineInstanceName)
			cancel()
			return Status{}, errors.Join(err, cleanupErr)
		}
		return Status{}, err
	}
	if created {
		if err := m.writeLimaOwnership(ctx, ownerID); err != nil {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, cleanupErr := m.runCommand(cleanupContext, 5*time.Minute, "clean up unowned Porto container runtime", nil, "limactl", "delete", "--force", engineInstanceName)
			cancel()
			return Status{}, errors.Join(err, cleanupErr)
		}
	} else if err := m.verifyLimaOwnership(ctx, ownerID); err != nil {
		return Status{}, err
	}
	limaBackend := commandBackend{
		name:        "limactl",
		prefix:      []string{"shell", engineInstanceName, "--", "nerdctl"},
		description: "containerd in Lima " + engineInstanceName,
	}
	versionOutput, err := m.runBackend(ctx, limaBackend, 30*time.Second, "verify Porto container runtime", nil, "version")
	if err != nil {
		if created {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, cleanupErr := m.runCommand(cleanupContext, 5*time.Minute, "clean up failed Porto container runtime", nil, "limactl", "delete", "--force", engineInstanceName)
			cancel()
			return Status{}, errors.Join(err, cleanupErr)
		}
		return Status{}, err
	}
	buildKit, err := m.dialBuildKitBackend(ctx, limaBackend)
	if err != nil {
		if created {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, cleanupErr := m.runCommand(cleanupContext, 5*time.Minute, "clean up Porto runtime without BuildKit", nil, "limactl", "delete", "--force", engineInstanceName)
			cancel()
			return Status{}, errors.Join(err, cleanupErr)
		}
		return Status{}, err
	}
	_ = buildKit.Close()
	if err := m.writeEngineState(engineState{
		Mode: "lima", Instance: engineInstanceName, OwnerID: ownerID, CreatedAt: time.Now().UTC(),
	}); err != nil {
		if created {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, cleanupErr := m.runCommand(cleanupContext, 5*time.Minute, "clean up untracked Porto container runtime", nil, "limactl", "delete", "--force", engineInstanceName)
			cancel()
			return Status{}, errors.Join(err, cleanupErr)
		}
		return Status{}, err
	}
	return installedStatus(limaBackend, versionOutput), nil
}

func (m *Manager) StartEngine(ctx context.Context) error {
	state, err := m.readEngineState()
	if err != nil {
		return fmt.Errorf("read Porto engine state: %w", err)
	}
	if state.Mode == "direct" {
		return errors.New("the direct nerdctl backend is managed outside Porto")
	}
	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Porto-owned Lima instance %q is missing", state.Instance)
	}
	if running {
		return m.verifyLimaOwnership(ctx, state.OwnerID)
	}
	if _, err := m.runCommand(ctx, 5*time.Minute, "start Porto container runtime", nil, "limactl", "start", state.Instance); err != nil {
		return err
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		_, stopErr := m.runCommand(context.Background(), 5*time.Minute, "stop unowned Lima instance", nil, "limactl", "stop", state.Instance)
		return errors.Join(err, stopErr)
	}
	return nil
}

func (m *Manager) StopEngine(ctx context.Context) error {
	state, err := m.readEngineState()
	if err != nil {
		return fmt.Errorf("read Porto engine state: %w", err)
	}
	if state.Mode == "direct" {
		return errors.New("the direct nerdctl backend is managed outside Porto")
	}
	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Porto-owned Lima instance %q is missing", state.Instance)
	}
	if !running {
		return nil
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return err
	}
	_, err = m.runCommand(ctx, 5*time.Minute, "stop Porto container runtime", nil, "limactl", "stop", state.Instance)
	return err
}

func (m *Manager) RemoveEngine(ctx context.Context) error {
	state, err := m.readEngineState()
	if err != nil {
		return fmt.Errorf("read Porto engine state: %w", err)
	}
	if state.Mode == "lima" {
		if err := m.ensureLimaOwnership(ctx, state); err != nil {
			return err
		}
		if _, err := m.runCommand(ctx, 5*time.Minute, "delete Porto container runtime", nil, "limactl", "delete", "--force", state.Instance); err != nil {
			return err
		}
	}
	if err := os.Remove(m.engineStatePath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Porto engine state: %w", err)
	}
	return nil
}

func (m *Manager) Containers(ctx context.Context) ([]Container, error) {
	return m.DockerContainers(ctx, true)
}

func (m *Manager) DockerContainers(ctx context.Context, all bool) ([]Container, error) {
	args := []string{"ps"}
	if all {
		args = append(args, "-a")
	}
	args = append(args, "--no-trunc", "--format", "{{json .}}")
	output, err := m.run(ctx, "list Porto containers", args...)
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Container {
		labels := parseLabels(item["Labels"])
		return Container{
			ID:             first(item, "ID", "Id"),
			Name:           first(item, "Names", "Name"),
			Image:          item["Image"],
			ImageID:        first(item, "ImageID", "ImageId"),
			Command:        item["Command"],
			State:          strings.ToLower(first(item, "State", "Status")),
			Status:         item["Status"],
			Ports:          item["Ports"],
			Networks:       item["Networks"],
			Mounts:         item["Mounts"],
			CreatedAt:      first(item, "CreatedAt", "Created"),
			Labels:         labels,
			ComposeProject: labels["com.docker.compose.project"],
			ComposeService: labels["com.docker.compose.service"],
		}
	})
}

func (m *Manager) CreateContainer(ctx context.Context, request CreateContainerRequest) (string, error) {
	if err := validateObjectID(request.Image); err != nil {
		return "", fmt.Errorf("image: %w", err)
	}
	hostname, err := containerHostname(request)
	if err != nil {
		return "", err
	}
	args := []string{"create"}
	if request.Name != "" {
		if err := validateObjectID(request.Name); err != nil {
			return "", fmt.Errorf("container name: %w", err)
		}
		args = append(args, "--name", request.Name)
	}
	args = appendStringFlag(args, "--platform", request.Platform)
	args = appendStringFlag(args, "--workdir", request.WorkingDir)
	args = appendStringFlag(args, "--user", request.User)
	args = appendStringFlag(args, "--hostname", hostname)
	args = appendStringFlag(args, "--stop-signal", request.StopSignal)
	if request.StopTimeout != nil {
		if *request.StopTimeout < 0 {
			return "", errors.New("container stop timeout cannot be negative")
		}
		args = append(args, "--stop-timeout", strconv.Itoa(*request.StopTimeout))
	}
	if request.Privileged {
		args = append(args, "--privileged")
	}
	for _, option := range request.SecurityOpt {
		if strings.TrimSpace(option) == "" || strings.ContainsAny(option, "\r\n\x00") {
			return "", errors.New("invalid container security option")
		}
		args = append(args, "--security-opt", option)
	}
	for _, target := range sortedStringKeys(request.Tmpfs) {
		value := target
		if request.Tmpfs[target] != "" {
			value += ":" + request.Tmpfs[target]
		}
		args = append(args, "--tmpfs", value)
	}
	for _, key := range sortedStringKeys(request.Sysctls) {
		args = append(args, "--sysctl", key+"="+request.Sysctls[key])
	}
	for _, device := range request.Devices {
		if err := validateContainerDevice(device); err != nil {
			return "", err
		}
		args = append(args, "--device", device.HostPath+":"+device.ContainerPath+":"+device.Permissions)
	}
	args = appendStringFlag(args, "--cgroupns", request.Cgroupns)
	args = appendStringFlag(args, "--userns", request.Userns)
	if request.Init {
		args = append(args, "--init")
	}
	if request.ShmSize > 0 {
		args = append(args, "--shm-size", strconv.FormatInt(request.ShmSize, 10))
	}
	for _, network := range request.Networks {
		if network.Name != "default" {
			args = appendStringFlag(args, "--network", network.Name)
		}
		for _, alias := range network.Aliases {
			if err := validateObjectID(alias); err != nil {
				return "", fmt.Errorf("network alias: %w", err)
			}
		}
	}
	args, err = appendHealthcheckArgs(args, request.Healthcheck)
	if err != nil {
		return "", err
	}
	if request.Restart != "no" {
		args = appendStringFlag(args, "--restart", request.Restart)
	}
	for _, entry := range request.Environment {
		args = append(args, "--env", entry)
	}
	for key, value := range request.Labels {
		args = append(args, "--label", key+"="+value)
	}
	for _, volume := range request.Volumes {
		args = append(args, "--volume", volume)
	}
	for _, published := range request.Publish {
		args = append(args, "--publish", published)
	}
	command := request.Command
	if len(request.Entrypoint) > 0 {
		args = append(args, "--entrypoint", request.Entrypoint[0])
		command = append(append([]string(nil), request.Entrypoint[1:]...), command...)
	}
	if request.TTY {
		args = append(args, "--tty")
	}
	if request.Interactive {
		args = append(args, "--interactive")
	}
	if request.Remove {
		args = append(args, "--rm")
	}
	args = append(args, normalizeNerdctlReference(request.Image))
	args = append(args, command...)
	output, err := m.runWithTimeout(ctx, 10*time.Minute, "create Porto container", nil, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(output))
	if id == "" {
		return "", errors.New("container runtime returned an empty container identifier")
	}
	return strings.Fields(id)[0], nil
}

func containerHostname(request CreateContainerRequest) (string, error) {
	aliases := make([]string, 0)
	seen := make(map[string]struct{})
	for _, network := range request.Networks {
		for _, alias := range network.Aliases {
			if err := validateObjectID(alias); err != nil {
				return "", fmt.Errorf("network alias: %w", err)
			}
			if alias == request.Name {
				continue
			}
			if _, ok := seen[alias]; !ok {
				seen[alias] = struct{}{}
				aliases = append(aliases, alias)
			}
		}
	}
	if request.Hostname != "" {
		for _, alias := range aliases {
			if alias != request.Hostname {
				return "", fmt.Errorf("%w: network aliases with an explicit hostname", ErrUnsupported)
			}
		}
		return request.Hostname, nil
	}
	if len(aliases) > 1 {
		return "", fmt.Errorf("%w: multiple network aliases", ErrUnsupported)
	}
	if len(aliases) == 1 {
		return aliases[0], nil
	}
	return "", nil
}

func appendHealthcheckArgs(args []string, healthcheck *ContainerHealthcheck) ([]string, error) {
	if healthcheck == nil {
		return args, nil
	}
	if healthcheck.StartInterval != 0 {
		return nil, fmt.Errorf("%w: healthcheck start interval", ErrUnsupported)
	}
	for name, value := range map[string]time.Duration{
		"interval":     healthcheck.Interval,
		"timeout":      healthcheck.Timeout,
		"start period": healthcheck.StartPeriod,
	} {
		if value < 0 {
			return nil, fmt.Errorf("healthcheck %s cannot be negative", name)
		}
	}
	if healthcheck.Retries < 0 {
		return nil, errors.New("healthcheck retries cannot be negative")
	}
	if len(healthcheck.Test) > 0 {
		switch healthcheck.Test[0] {
		case "NONE":
			return append(args, "--no-healthcheck"), nil
		case "CMD":
			if len(healthcheck.Test) < 2 {
				return nil, errors.New("healthcheck CMD requires a command")
			}
			args = append(args, "--health-cmd", shellJoin(healthcheck.Test[1:]))
		case "CMD-SHELL":
			if len(healthcheck.Test) < 2 {
				return nil, errors.New("healthcheck CMD-SHELL requires a command")
			}
			args = append(args, "--health-cmd", strings.Join(healthcheck.Test[1:], " "))
		default:
			return nil, fmt.Errorf("%w: healthcheck test type %q", ErrUnsupported, healthcheck.Test[0])
		}
	}
	if healthcheck.Interval > 0 {
		args = append(args, "--health-interval", healthcheck.Interval.String())
	}
	if healthcheck.Timeout > 0 {
		args = append(args, "--health-timeout", healthcheck.Timeout.String())
	}
	if healthcheck.StartPeriod > 0 {
		args = append(args, "--health-start-period", healthcheck.StartPeriod.String())
	}
	if healthcheck.Retries > 0 {
		args = append(args, "--health-retries", strconv.Itoa(healthcheck.Retries))
	}
	return args, nil
}

func shellJoin(command []string) string {
	quoted := make([]string, 0, len(command))
	for _, value := range command {
		if value == "" {
			quoted = append(quoted, "''")
			continue
		}
		if !strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>(){}[]*?!~") {
			quoted = append(quoted, value)
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(value, "'", "'\\''")+"'")
	}
	return strings.Join(quoted, " ")
}

func validateContainerDevice(device ContainerDevice) error {
	for name, value := range map[string]string{
		"host path":      device.HostPath,
		"container path": device.ContainerPath,
		"permissions":    device.Permissions,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00:") {
			return fmt.Errorf("invalid container device %s", name)
		}
	}
	return nil
}

func (m *Manager) Images(ctx context.Context) ([]Image, error) {
	output, err := m.run(ctx, "list Porto images", "images", "--digests", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Image {
		return Image{
			ID:         first(item, "ID", "Id"),
			Repository: item["Repository"],
			Tag:        item["Tag"],
			Digest:     item["Digest"],
			Size:       item["Size"],
			CreatedAt:  first(item, "CreatedAt", "CreatedSince", "Created"),
			Labels:     parseLabels(item["Labels"]),
		}
	})
}

func (m *Manager) Networks(ctx context.Context) ([]Network, error) {
	output, err := m.run(ctx, "list Porto networks", "network", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Network {
		return Network{
			ID:       first(item, "ID", "Id"),
			Name:     item["Name"],
			Driver:   item["Driver"],
			Scope:    first(item, "Scope", "DockerScope"),
			Internal: item["Internal"],
			IPv6:     first(item, "IPv6", "EnableIPv6"),
			Created:  first(item, "CreatedAt", "Created"),
			Labels:   parseLabels(item["Labels"]),
		}
	})
}

func (m *Manager) Volumes(ctx context.Context) ([]Volume, error) {
	output, err := m.run(ctx, "list Porto volumes", "volume", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Volume {
		return Volume{
			Name:       item["Name"],
			Driver:     item["Driver"],
			Mountpoint: item["Mountpoint"],
			Scope:      item["Scope"],
			CreatedAt:  first(item, "CreatedAt", "Created"),
			Labels:     parseLabels(item["Labels"]),
		}
	})
}

func (m *Manager) ContainerAction(ctx context.Context, id, action string) error {
	return m.ContainerActionWithTimeout(ctx, id, action, 0)
}

func (m *Manager) ContainerActionWithTimeout(ctx context.Context, id, action string, timeout int) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	var args []string
	switch action {
	case "start", "pause", "unpause":
		args = []string{action, id}
	case "stop", "restart":
		args = []string{action}
		if timeout > 0 {
			args = append(args, "--time", strconv.Itoa(timeout))
		}
		args = append(args, id)
	case "remove":
		args = []string{"rm", id}
	case "remove-force":
		args = []string{"rm", "--force", id}
	case "remove-volumes":
		args = []string{"rm", "--volumes", id}
	case "remove-force-volumes":
		args = []string{"rm", "--force", "--volumes", id}
	default:
		return fmt.Errorf("unsupported container action %q", action)
	}
	_, err := m.run(ctx, action+" Porto container", args...)
	return err
}

func (m *Manager) RenameContainer(ctx context.Context, id, name string) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	if err := validateObjectID(name); err != nil {
		return err
	}
	_, err := m.run(ctx, "rename Porto container", "rename", id, name)
	return err
}

func (m *Manager) WaitContainer(ctx context.Context, id, condition string) (int, error) {
	if err := validateObjectID(id); err != nil {
		return 0, err
	}
	if condition != "" && condition != "not-running" {
		return 0, fmt.Errorf("unsupported wait condition %q", condition)
	}
	output, err := m.runWithTimeout(ctx, 24*time.Hour, "wait for Porto container", nil, "wait", id)
	if err != nil {
		return 0, err
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 0, fmt.Errorf("decode container exit code: %w", err)
	}
	return code, nil
}

func (m *Manager) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	document, err := m.inspect(ctx, "container", id)
	if err != nil {
		return nil, err
	}
	due, timeout, err := healthcheckDue(document, time.Now())
	if err != nil {
		return nil, err
	}
	if !due {
		return document, nil
	}
	m.healthMu.Lock()
	defer m.healthMu.Unlock()
	document, err = m.inspect(ctx, "container", id)
	if err != nil {
		return nil, err
	}
	due, timeout, err = healthcheckDue(document, time.Now())
	if err != nil || !due {
		return document, err
	}
	if _, err := m.runWithTimeout(ctx, timeout+5*time.Second, "refresh Porto container health", nil, "healthcheck", id); err != nil {
		return nil, err
	}
	return m.inspect(ctx, "container", id)
}

func healthcheckDue(document json.RawMessage, now time.Time) (bool, time.Duration, error) {
	var inspected struct {
		Config struct {
			Healthcheck *struct {
				Test     []string `json:"Test"`
				Interval int64    `json:"Interval"`
				Timeout  int64    `json:"Timeout"`
			} `json:"Healthcheck"`
		} `json:"Config"`
		State struct {
			Running bool `json:"Running"`
			Health  *struct {
				Log []struct {
					End time.Time `json:"End"`
				} `json:"Log"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return false, 0, fmt.Errorf("decode container health settings: %w", err)
	}
	healthcheck := inspected.Config.Healthcheck
	if !inspected.State.Running || healthcheck == nil || len(healthcheck.Test) == 0 || healthcheck.Test[0] == "NONE" {
		return false, 0, nil
	}
	timeout := time.Duration(healthcheck.Timeout)
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if inspected.State.Health == nil || len(inspected.State.Health.Log) == 0 {
		return true, timeout, nil
	}
	interval := time.Duration(healthcheck.Interval)
	if interval <= 0 {
		interval = 30 * time.Second
	}
	var lastCheck time.Time
	for _, result := range inspected.State.Health.Log {
		if result.End.After(lastCheck) {
			lastCheck = result.End
		}
	}
	return lastCheck.IsZero() || !now.Before(lastCheck.Add(interval)), timeout, nil
}

func (m *Manager) ContainerTTY(ctx context.Context, id string) (bool, error) {
	document, err := m.InspectContainer(ctx, id)
	if err != nil {
		return false, err
	}
	var inspected struct {
		Config struct {
			TTY bool `json:"Tty"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return false, fmt.Errorf("decode container terminal settings: %w", err)
	}
	return inspected.Config.TTY, nil
}

func (m *Manager) ExecContainer(ctx context.Context, id string, command []string, stdin []byte) ([]byte, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("container command is required")
	}
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "--interactive")
	}
	args = append(args, id)
	args = append(args, command...)
	return m.runWithTimeout(ctx, 30*time.Minute, "execute Porto container command", stdin, args...)
}

func (m *Manager) ContainerStats(ctx context.Context) ([]ContainerStats, error) {
	output, err := m.run(ctx, "read Porto container stats", "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) ContainerStats {
		return ContainerStats{
			ID:       item["ID"],
			Name:     item["Name"],
			CPU:      first(item, "CPUPerc", "CPU"),
			Memory:   first(item, "MemUsage", "Memory"),
			MemoryPC: first(item, "MemPerc", "MemoryPercent"),
			Network:  first(item, "NetIO", "Network"),
			BlockIO:  first(item, "BlockIO", "Block"),
			PIDs:     item["PIDs"],
		}
	})
}

func (m *Manager) InspectImage(ctx context.Context, id string) (json.RawMessage, error) {
	return m.inspect(ctx, "image", normalizeNerdctlReference(id))
}

func (m *Manager) InspectNetwork(ctx context.Context, id string) (json.RawMessage, error) {
	return m.inspect(ctx, "network", id)
}

func (m *Manager) InspectVolume(ctx context.Context, id string) (json.RawMessage, error) {
	return m.inspect(ctx, "volume", id)
}

func (m *Manager) RemoveImage(ctx context.Context, id string, force bool) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	args := []string{"image", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, normalizeNerdctlReference(id))
	_, err := m.run(ctx, "remove Porto image", args...)
	return err
}

func (m *Manager) PullImage(ctx context.Context, reference, platform string) error {
	if err := validateObjectID(reference); err != nil {
		return err
	}
	args := []string{"pull"}
	args = appendStringFlag(args, "--platform", platform)
	normalized := normalizeNerdctlReference(reference)
	args = append(args, normalized)
	_, err := m.runWithTimeout(ctx, 30*time.Minute, "pull Porto image", nil, args...)
	if err != nil {
		return fmt.Errorf("pull image %q: %w", normalized, err)
	}
	return nil
}

func (m *Manager) InstallContext(ctx context.Context, socketPath string) error {
	endpoint := dockerEndpoint(socketPath)
	if _, err := m.runDockerCLI(ctx, "inspect Porto Docker context", "context", "inspect", "porto"); err == nil {
		_, err = m.runDockerCLI(ctx, "update Porto Docker context", "context", "update", "porto", "--docker", "host="+endpoint)
		return err
	}
	_, err := m.runDockerCLI(ctx, "create Porto Docker context", "context", "create", "porto", "--docker", "host="+endpoint)
	return err
}

func (m *Manager) inspect(ctx context.Context, kind, id string) (json.RawMessage, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	output, err := m.run(ctx, "inspect Porto "+kind, kind, "inspect", id)
	if err != nil {
		return nil, err
	}
	var values []json.RawMessage
	if err := json.Unmarshal(output, &values); err == nil && len(values) > 0 {
		return values[0], nil
	}
	if !json.Valid(output) {
		return nil, fmt.Errorf("Porto %s inspect returned invalid JSON", kind)
	}
	return json.RawMessage(output), nil
}

type commandBackend struct {
	name        string
	prefix      []string
	description string
}

func (m *Manager) backend(ctx context.Context) (commandBackend, error) {
	if m.directCLI {
		return commandBackend{name: "nerdctl", description: "nerdctl"}, nil
	}
	state, err := m.readEngineState()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return commandBackend{}, fmt.Errorf("read Porto engine state: %w", err)
		}
		if path, pathErr := m.lookPath("nerdctl"); pathErr == nil {
			return commandBackend{name: path, description: "containerd via nerdctl"}, nil
		}
		return commandBackend{}, fmt.Errorf("%w; run 'porto docker engine-install'", ErrUnavailable)
	}
	if state.Mode == "direct" {
		if path, pathErr := m.lookPath("nerdctl"); pathErr == nil {
			return commandBackend{name: path, description: "containerd via nerdctl"}, nil
		}
		return commandBackend{}, fmt.Errorf("%w; direct nerdctl is no longer available", ErrUnavailable)
	}
	if state.Mode != "lima" || state.Instance == "" {
		return commandBackend{}, fmt.Errorf("%w; engine state has unsupported mode %q", ErrUnavailable, state.Mode)
	}
	if _, err := m.lookPath("limactl"); err != nil {
		return commandBackend{}, fmt.Errorf("%w; limactl is not installed", ErrUnavailable)
	}
	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return commandBackend{}, err
	}
	if !exists {
		return commandBackend{}, fmt.Errorf("%w; the Porto Lima instance is missing, run 'porto docker engine-install'", ErrUnavailable)
	}
	if !running {
		return commandBackend{}, fmt.Errorf("%w; run 'porto docker engine-start'", ErrUnavailable)
	}
	if err := m.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		return commandBackend{}, err
	}
	return commandBackend{
		name:        "limactl",
		prefix:      []string{"shell", state.Instance, "--", "nerdctl"},
		description: "containerd in Lima " + state.Instance,
	}, nil
}

func (m *Manager) run(ctx context.Context, action string, args ...string) ([]byte, error) {
	return m.runWithTimeout(ctx, m.timeout, action, nil, args...)
}

func (m *Manager) runWithTimeout(ctx context.Context, timeout time.Duration, action string, stdin []byte, args ...string) ([]byte, error) {
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	return m.runBackend(ctx, backend, timeout, action, stdin, args...)
}

func (m *Manager) runBackend(
	ctx context.Context,
	backend commandBackend,
	timeout time.Duration,
	action string,
	stdin []byte,
	args ...string,
) ([]byte, error) {
	return m.runCommand(ctx, timeout, action, stdin, backend.name, append(append([]string(nil), backend.prefix...), args...)...)
}

func (m *Manager) runDockerCLI(ctx context.Context, action string, args ...string) ([]byte, error) {
	return m.runCommand(ctx, m.timeout, action, nil, "docker", args...)
}

type streamingRunner interface {
	RunStreaming(context.Context, runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error)
}

func (m *Manager) runStreaming(
	ctx context.Context,
	timeout time.Duration,
	action string,
	stdin []byte,
	emit func(runtimes.OutputChunk) error,
	args ...string,
) error {
	return m.runStreamingInput(ctx, timeout, action, stdin, nil, emit, args...)
}

func (m *Manager) runStreamingReader(
	ctx context.Context,
	timeout time.Duration,
	action string,
	stdinReader io.Reader,
	emit func(runtimes.OutputChunk) error,
	args ...string,
) error {
	return m.runStreamingInput(ctx, timeout, action, nil, stdinReader, emit, args...)
}

func (m *Manager) runStreamingInput(
	ctx context.Context,
	timeout time.Duration,
	action string,
	stdin []byte,
	stdinReader io.Reader,
	emit func(runtimes.OutputChunk) error,
	args ...string,
) error {
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	runner, ok := m.runner.(streamingRunner)
	if !ok {
		return fmt.Errorf("%w: streaming stdout and stderr capture", ErrUnsupported)
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := runtimes.Command{
		Name:        backend.name,
		Args:        append(append([]string(nil), backend.prefix...), args...),
		Stdin:       stdin,
		StdinReader: stdinReader,
	}
	output, runErr := runner.RunStreaming(commandContext, command, emit)
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out after %s", action, timeout)
	}
	if runErr != nil {
		return runtimes.CommandError(action, output, runErr)
	}
	return nil
}

func (m *Manager) runCommand(
	ctx context.Context,
	timeout time.Duration,
	action string,
	stdin []byte,
	name string,
	args ...string,
) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: name, Args: args, Stdin: stdin})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", action, timeout)
	}
	if err != nil {
		return nil, runtimes.CommandError(action, output, err)
	}
	return output, nil
}

func (m *Manager) limaInstanceStatus(ctx context.Context) (exists bool, running bool, err error) {
	output, err := m.runCommand(ctx, 20*time.Second, "inspect Porto container runtime", nil, "limactl", "list", "--json")
	if err != nil {
		return false, false, err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			var items []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}
			if batchErr := json.Unmarshal([]byte(line), &items); batchErr != nil {
				return false, false, fmt.Errorf("decode Lima instance list: %w", err)
			}
			for _, candidate := range items {
				if candidate.Name == engineInstanceName {
					return true, strings.EqualFold(candidate.Status, "running"), nil
				}
			}
			continue
		}
		if item.Name == engineInstanceName {
			return true, strings.EqualFold(item.Status, "running"), nil
		}
	}
	return false, false, scanner.Err()
}

func (m *Manager) writeLimaOwnership(ctx context.Context, ownerID string) error {
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("Porto engine owner identifier is empty")
	}
	_, err := m.runCommand(
		ctx,
		30*time.Second,
		"write Porto engine ownership marker",
		[]byte(ownerID+"\n"),
		"limactl",
		"shell",
		engineInstanceName,
		"--",
		"sh",
		"-c",
		`umask 077; cat > "$HOME/.porto-engine-owner"`,
	)
	return err
}

func (m *Manager) verifyLimaOwnership(ctx context.Context, ownerID string) error {
	if strings.TrimSpace(ownerID) == "" {
		return errors.New("Porto engine ownership metadata is incomplete")
	}
	output, err := m.runCommand(
		ctx,
		30*time.Second,
		"verify Porto engine ownership",
		nil,
		"limactl",
		"shell",
		engineInstanceName,
		"--",
		"sh",
		"-c",
		`cat "$HOME/.porto-engine-owner"`,
	)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(output)) != ownerID {
		return fmt.Errorf("refusing to manage Lima instance %q because its Porto ownership marker does not match", engineInstanceName)
	}
	return nil
}

func (m *Manager) ensureLimaOwnership(ctx context.Context, state engineState) error {
	exists, running, err := m.limaInstanceStatus(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Porto-owned Lima instance %q is missing", state.Instance)
	}
	if !running {
		if _, err := m.runCommand(ctx, 5*time.Minute, "start Porto container runtime for ownership verification", nil, "limactl", "start", state.Instance); err != nil {
			return err
		}
	}
	return m.verifyLimaOwnership(ctx, state.OwnerID)
}

func (m *Manager) engineStatePath() string {
	return filepath.Join(m.stateDir, engineStateFile)
}

func (m *Manager) readEngineState() (engineState, error) {
	data, err := os.ReadFile(m.engineStatePath())
	if err != nil {
		return engineState{}, err
	}
	var state engineState
	if err := json.Unmarshal(data, &state); err != nil {
		return engineState{}, fmt.Errorf("decode Porto engine state: %w", err)
	}
	return state, nil
}

func (m *Manager) writeEngineState(state engineState) error {
	if m.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(m.stateDir, 0o700); err != nil {
		return fmt.Errorf("create Porto engine state directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Porto engine state: %w", err)
	}
	temp, err := os.CreateTemp(m.stateDir, ".engine.*")
	if err != nil {
		return fmt.Errorf("create Porto engine state: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect Porto engine state: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write Porto engine state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync Porto engine state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close Porto engine state: %w", err)
	}
	if err := os.Rename(tempPath, m.engineStatePath()); err != nil {
		return fmt.Errorf("install Porto engine state: %w", err)
	}
	return nil
}
