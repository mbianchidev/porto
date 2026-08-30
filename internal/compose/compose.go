package compose

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/ports"
)

const (
	defaultCheckTimeout = 5 * time.Second
	defaultDownTimeout  = 2 * time.Minute
	runtimeGuidance     = "start or repair the configured Docker-compatible runtime, then retry"
)

var ErrDaemonUnavailable = errors.New("docker daemon is unavailable to Porto")

var configFiles = []string{
	"docker-compose.yml",
	"docker-compose.yaml",
	"compose.yml",
	"compose.yaml",
}

type Command struct {
	Dir  string
	Name string
	Args []string
	Env  []string
}

type PublishedPort struct {
	Service string
	Port    int
}

type PlannedPort struct {
	Service   string
	Target    int
	Published int
	Protocol  string
}

type PortPlan struct {
	ConfigFile   string
	OverridePath string
	Ports        []PlannedPort
}

type Runner interface {
	Run(ctx context.Context, command Command) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append(os.Environ(), command.Env...)
	return cmd.CombinedOutput()
}

type Integration struct {
	runner       Runner
	checkTimeout time.Duration
	downTimeout  time.Duration
}

func New(runner Runner) *Integration {
	return newIntegration(runner, defaultDownTimeout)
}

func newIntegration(runner Runner, timeout time.Duration) *Integration {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Integration{
		runner:       runner,
		checkTimeout: defaultCheckTimeout,
		downTimeout:  timeout,
	}
}

func FindFile(root string) (string, bool) {
	for _, name := range configFiles {
		info, err := os.Stat(filepath.Join(root, name))
		if err == nil && info.Mode().IsRegular() {
			return name, true
		}
	}
	return "", false
}

func UpCommand(file string) string {
	return "docker compose -f " + file + " up"
}

func UpCommandWithOverride(file, override string) string {
	return "docker compose -f " + shellQuote(file) + " -f " + shellQuote(override) + " up"
}

func (i *Integration) Check(ctx context.Context) error {
	commandContext, cancel := context.WithTimeout(ctx, i.checkTimeout)
	defer cancel()
	output, err := i.runner.Run(commandContext, Command{
		Name: "docker",
		Args: []string{"info", "--format", "{{.ServerVersion}}"},
	})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: Docker availability check timed out; %s", ErrDaemonUnavailable, runtimeGuidance)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("docker is not available in the Porto daemon PATH: %w", err)
		}
		return daemonUnavailableError(output, err)
	}
	if strings.TrimSpace(string(output)) == "" {
		return fmt.Errorf("%w: docker info returned no server version; %s", ErrDaemonUnavailable, runtimeGuidance)
	}
	return nil
}

func (i *Integration) PublishedPorts(ctx context.Context, project app.Project) ([]PublishedPort, error) {
	file, err := configFile(project)
	if err != nil {
		return nil, err
	}
	commandContext, cancel := context.WithTimeout(ctx, i.checkTimeout)
	defer cancel()
	args := []string{"compose", "-f", file}
	if override, overrideErr := OverridePath(project); overrideErr == nil {
		if info, statErr := os.Stat(override); statErr == nil && !info.IsDir() {
			args = append(args, "-f", override)
		}
	}
	args = append(args, "ps", "--format", "json")
	output, err := i.runner.Run(commandContext, Command{
		Dir:  project.Path,
		Name: "docker",
		Args: args,
		Env: []string{
			"PORT=" + strconv.Itoa(project.Port),
			"PORTO_PORT=" + strconv.Itoa(project.Port),
			"COMPOSE_PROJECT_NAME=" + ProjectName(project),
		},
	})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("docker compose port discovery for %s timed out: %w", project.Name, commandContext.Err())
	}
	if err != nil {
		return nil, commandError("docker compose port discovery for "+project.Name, output, err)
	}
	return parsePublishedPorts(output)
}

func (i *Integration) Down(ctx context.Context, project app.Project) error {
	file, err := configFile(project)
	if err != nil {
		return err
	}
	commandContext, cancel := context.WithTimeout(ctx, i.downTimeout)
	defer cancel()
	args := []string{"compose", "-f", file}
	override, overrideErr := OverridePath(project)
	if overrideErr == nil {
		if info, statErr := os.Stat(override); statErr == nil && !info.IsDir() {
			args = append(args, "-f", override)
		}
	}
	args = append(args, "down", "--remove-orphans")
	output, err := i.runner.Run(commandContext, Command{
		Dir:  project.Path,
		Name: "docker",
		Args: args,
		Env: []string{
			"PORT=" + strconv.Itoa(project.Port),
			"PORTO_PORT=" + strconv.Itoa(project.Port),
			"COMPOSE_PROJECT_NAME=" + ProjectName(project),
		},
	})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("docker compose cleanup for %s timed out: %w", project.Name, commandContext.Err())
	}
	if err != nil {
		return commandError("docker compose cleanup for "+project.Name, output, err)
	}
	if overrideErr == nil {
		if err := os.Remove(override); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove Docker Compose port override: %w", err)
		}
	}
	return nil
}

func (i *Integration) PreparePorts(ctx context.Context, project app.Project, used map[int]bool) (PortPlan, error) {
	file, err := configFile(project)
	if err != nil {
		return PortPlan{}, err
	}
	commandContext, cancel := context.WithTimeout(ctx, i.checkTimeout)
	defer cancel()
	output, err := i.runner.Run(commandContext, Command{
		Dir:  project.Path,
		Name: "docker",
		Args: []string{"compose", "-f", file, "config", "--format", "json"},
		Env:  []string{"COMPOSE_PROJECT_NAME=" + ProjectName(project)},
	})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return PortPlan{}, fmt.Errorf("Docker Compose port planning for %s timed out", project.Name)
	}
	if err != nil {
		return PortPlan{}, commandError("inspect Docker Compose ports for "+project.Name, output, err)
	}
	var composeConfig struct {
		Services map[string]struct {
			Ports []struct {
				Target    int    `json:"target"`
				Published string `json:"published"`
				Protocol  string `json:"protocol"`
			} `json:"ports"`
		} `json:"services"`
	}
	if err := json.Unmarshal(output, &composeConfig); err != nil {
		return PortPlan{}, fmt.Errorf("decode Docker Compose configuration for %s: %w", project.Name, err)
	}
	serviceNames := make([]string, 0, len(composeConfig.Services))
	for service := range composeConfig.Services {
		serviceNames = append(serviceNames, service)
	}
	sort.Slice(serviceNames, func(left, right int) bool {
		leftPriority := servicePriority(serviceNames[left])
		rightPriority := servicePriority(serviceNames[right])
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return serviceNames[left] < serviceNames[right]
	})
	plan := PortPlan{ConfigFile: file}
	for _, service := range serviceNames {
		for _, configured := range composeConfig.Services[service].Ports {
			if configured.Target <= 0 || (configured.Protocol != "" && configured.Protocol != "tcp") {
				continue
			}
			preferred, _ := strconv.Atoi(configured.Published)
			published, err := ports.Pick(preferred, config.BasePort, used)
			if err != nil {
				return PortPlan{}, fmt.Errorf("allocate Docker Compose port for %s/%s: %w", project.Name, service, err)
			}
			used[published] = true
			protocol := configured.Protocol
			if protocol == "" {
				protocol = "tcp"
			}
			plan.Ports = append(plan.Ports, PlannedPort{
				Service: service, Target: configured.Target, Published: published, Protocol: protocol,
			})
		}
	}
	override, err := OverridePath(project)
	if err != nil {
		return PortPlan{}, err
	}
	plan.OverridePath = override
	if len(plan.Ports) == 0 {
		if err := os.Remove(override); err != nil && !errors.Is(err, os.ErrNotExist) {
			return PortPlan{}, err
		}
		return plan, nil
	}
	if err := writePortOverride(override, plan.Ports); err != nil {
		return PortPlan{}, err
	}
	return plan, nil
}

type composeProcess struct {
	Service    string `json:"Service"`
	State      string `json:"State"`
	Publishers []struct {
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

func parsePublishedPorts(output []byte) ([]PublishedPort, error) {
	var processes []composeProcess
	var parseErr error
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch line[0] {
		case '{':
			var process composeProcess
			if err := json.Unmarshal([]byte(line), &process); err != nil {
				parseErr = errors.Join(parseErr, err)
				continue
			}
			processes = append(processes, process)
		case '[':
			var batch []composeProcess
			if err := json.Unmarshal([]byte(line), &batch); err != nil {
				parseErr = errors.Join(parseErr, err)
				continue
			}
			processes = append(processes, batch...)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(processes) == 0 && parseErr != nil {
		return nil, fmt.Errorf("parse docker compose service ports: %w", parseErr)
	}

	seen := make(map[string]struct{})
	ports := make([]PublishedPort, 0)
	for _, process := range processes {
		if process.State != "" && process.State != "running" {
			continue
		}
		for _, publisher := range process.Publishers {
			if publisher.PublishedPort <= 0 || (publisher.Protocol != "" && publisher.Protocol != "tcp") {
				continue
			}
			key := process.Service + ":" + strconv.Itoa(publisher.PublishedPort)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ports = append(ports, PublishedPort{Service: process.Service, Port: publisher.PublishedPort})
		}
	}
	sort.SliceStable(ports, func(left, right int) bool {
		leftPriority := servicePriority(ports[left].Service)
		rightPriority := servicePriority(ports[right].Service)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		return ports[left].Port < ports[right].Port
	})
	return ports, nil
}

func servicePriority(service string) int {
	name := strings.ToLower(service)
	for priority, names := range [][]string{
		{"frontend", "web", "ui", "client"},
		{"app"},
		{"api", "backend", "server"},
	} {
		for _, candidate := range names {
			if name == candidate || strings.HasPrefix(name, candidate+"-") || strings.HasSuffix(name, "-"+candidate) {
				return priority
			}
		}
	}
	return 3
}

func ProjectName(project app.Project) string {
	if project.ID > 0 {
		return "porto-" + strconv.FormatInt(project.ID, 10)
	}
	name := strings.ToLower(project.Name)
	var builder strings.Builder
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}
	normalized := strings.Trim(builder.String(), "-_")
	if normalized == "" {
		normalized = "project"
	}
	return "porto-" + normalized
}

func OverridePath(project app.Project) (string, error) {
	runtimeDir, err := config.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(runtimeDir, "compose", ProjectName(project)+".ports.yaml"), nil
}

func writePortOverride(path string, planned []PlannedPort) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Docker Compose runtime directory: %w", err)
	}
	services := make(map[string][]PlannedPort)
	for _, port := range planned {
		services[port.Service] = append(services[port.Service], port)
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	var builder strings.Builder
	builder.WriteString("services:\n")
	for _, name := range names {
		builder.WriteString("  ")
		builder.WriteString(strconv.Quote(name))
		builder.WriteString(":\n    ports: !override\n")
		for _, port := range services[name] {
			builder.WriteString("      - ")
			builder.WriteString(strconv.Quote(fmt.Sprintf("127.0.0.1:%d:%d/%s", port.Published, port.Target, port.Protocol)))
			builder.WriteByte('\n')
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".compose-ports.*")
	if err != nil {
		return fmt.Errorf("create Docker Compose port override: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.WriteString(builder.String()); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write Docker Compose port override: %w", err)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install Docker Compose port override: %w", err)
	}
	return nil
}

func shellQuote(value string) string {
	if !strings.ContainsAny(value, " \t'\"") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func daemonUnavailableError(output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%w: %s: %v", ErrDaemonUnavailable, runtimeGuidance, err)
	}
	return fmt.Errorf("%w: %s: %s", ErrDaemonUnavailable, runtimeGuidance, message)
}

func configFile(project app.Project) (string, error) {
	const prefix = "docker compose -f "
	const suffix = " up"
	if strings.HasPrefix(project.Command, prefix) && strings.HasSuffix(project.Command, suffix) {
		file := strings.TrimSpace(project.Command[len(prefix) : len(project.Command)-len(suffix)])
		if file != "" {
			return file, nil
		}
	}
	if file, ok := FindFile(project.Path); ok {
		return file, nil
	}
	return "", fmt.Errorf("no Docker Compose file found for %s", project.Name)
}

func commandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s failed: %w", action, err)
	}
	return fmt.Errorf("%s failed: %w: %s", action, err, message)
}
