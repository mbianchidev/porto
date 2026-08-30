package vm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const defaultTimeout = 2 * time.Minute

var (
	ErrUnavailable = errors.New("Lima is unavailable; install limactl to manage Porto virtual machines")
	vmNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
)

type Manager struct {
	runner  runtimes.Runner
	timeout time.Duration
}

type Status struct {
	Enabled   bool   `json:"enabled"`
	Available bool   `json:"available"`
	Provider  string `json:"provider"`
	Version   string `json:"version,omitempty"`
	Message   string `json:"message,omitempty"`
}

type Image struct {
	ID           string `json:"id"`
	Distribution string `json:"distribution"`
	Version      string `json:"version"`
	Template     string `json:"template"`
	Description  string `json:"description"`
}

type Instance struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Architecture string   `json:"architecture"`
	CPUs         int      `json:"cpus"`
	MemoryBytes  int64    `json:"memoryBytes"`
	DiskBytes    int64    `json:"diskBytes"`
	SSHLocalPort int      `json:"sshLocalPort"`
	Directory    string   `json:"directory"`
	Addresses    []string `json:"addresses"`
}

type CreateRequest struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	CPUs         int    `json:"cpus"`
	MemoryMiB    int    `json:"memoryMiB"`
	DiskGiB      int    `json:"diskGiB"`
	Architecture string `json:"architecture"`
	Provision    string `json:"provision"`
	Start        bool   `json:"start"`
}

func New(runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{runner: runner, timeout: defaultTimeout}
}

func (m *Manager) Status(ctx context.Context) Status {
	output, err := m.run(ctx, 10*time.Second, "inspect Lima version", "--version")
	if err != nil {
		return Status{Provider: "lima", Message: err.Error()}
	}
	return Status{
		Available: true,
		Provider:  "lima",
		Version:   strings.TrimSpace(string(output)),
	}
}

func (m *Manager) Images() []Image {
	return []Image{
		{ID: "ubuntu-24.04", Distribution: "Ubuntu", Version: "24.04 LTS", Template: "template://ubuntu-24.04", Description: "General-purpose Ubuntu LTS development machine"},
		{ID: "centos-stream-10", Distribution: "CentOS Stream", Version: "10", Template: "template://centos-stream-10", Description: "CentOS Stream server-compatible environment"},
		{ID: "opensuse-tumbleweed", Distribution: "openSUSE", Version: "Tumbleweed", Template: "template://opensuse-tumbleweed", Description: "Rolling openSUSE development environment"},
		{ID: "nixos-unstable", Distribution: "NixOS", Version: "Unstable", Template: "template://experimental/nixos", Description: "Declarative NixOS test environment"},
		{ID: "archlinux", Distribution: "Arch Linux", Version: "Rolling", Template: "template://archlinux", Description: "Minimal rolling Arch Linux environment"},
		{ID: "alpine", Distribution: "Alpine Linux", Version: "Latest", Template: "template://alpine", Description: "Small musl-based Linux environment"},
		{ID: "kali", Distribution: "Kali Linux", Version: "Rolling", Template: "template://kali", Description: "Security testing environment"},
	}
}

func (m *Manager) List(ctx context.Context) ([]Instance, error) {
	output, err := m.run(ctx, 20*time.Second, "list Lima instances", "list", "--json")
	if err != nil {
		return nil, err
	}
	instances := make([]Instance, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			var batch []map[string]any
			if batchErr := json.Unmarshal([]byte(line), &batch); batchErr != nil {
				return nil, fmt.Errorf("decode Lima instance: %w", err)
			}
			for _, item := range batch {
				instances = append(instances, decodeInstance(item))
			}
			continue
		}
		instances = append(instances, decodeInstance(raw))
	}
	return instances, scanner.Err()
}

func (m *Manager) Create(ctx context.Context, request CreateRequest) (Instance, error) {
	if err := validateName(request.Name); err != nil {
		return Instance{}, err
	}
	image, ok := m.image(request.Image)
	if !ok {
		return Instance{}, fmt.Errorf("unsupported VM image %q", request.Image)
	}
	if request.CPUs <= 0 {
		request.CPUs = 2
	}
	if request.MemoryMiB <= 0 {
		request.MemoryMiB = 2048
	}
	if request.DiskGiB <= 0 {
		request.DiskGiB = 20
	}
	args := []string{
		"create",
		"--tty=false",
		"--name", request.Name,
		"--cpus", strconv.Itoa(request.CPUs),
		"--memory", strconv.Itoa(request.MemoryMiB) + "MiB",
		"--disk", strconv.Itoa(request.DiskGiB) + "GiB",
	}
	if request.Architecture != "" {
		if request.Architecture != "aarch64" && request.Architecture != "x86_64" {
			return Instance{}, fmt.Errorf("unsupported VM architecture %q", request.Architecture)
		}
		args = append(args, "--arch", request.Architecture)
	}
	args = append(args, image.Template)
	if _, err := m.run(ctx, 10*time.Minute, "create Lima instance", args...); err != nil {
		return Instance{}, err
	}
	if request.Start {
		if err := m.Start(ctx, request.Name); err != nil {
			return Instance{}, err
		}
		if strings.TrimSpace(request.Provision) != "" {
			if _, err := m.Exec(ctx, request.Name, []string{"sh", "-lc", request.Provision}, nil); err != nil {
				return Instance{}, fmt.Errorf("provision VM %s: %w", request.Name, err)
			}
		}
	}
	return m.Get(ctx, request.Name)
}

func (m *Manager) Get(ctx context.Context, name string) (Instance, error) {
	if err := validateName(name); err != nil {
		return Instance{}, err
	}
	instances, err := m.List(ctx)
	if err != nil {
		return Instance{}, err
	}
	for _, instance := range instances {
		if instance.Name == name {
			return instance, nil
		}
	}
	return Instance{}, fmt.Errorf("VM %q not found", name)
}

func (m *Manager) Start(ctx context.Context, name string) error {
	return m.action(ctx, "start", name)
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	return m.action(ctx, "stop", name)
}

func (m *Manager) Delete(ctx context.Context, name string, force bool) error {
	if err := validateName(name); err != nil {
		return err
	}
	args := []string{"delete"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, err := m.run(ctx, 5*time.Minute, "delete Lima instance", args...)
	return err
}

func (m *Manager) Exec(ctx context.Context, name string, command []string, stdin []byte) ([]byte, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("VM command is required")
	}
	args := append([]string{"shell", name, "--"}, command...)
	commandContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "limactl", Args: args, Stdin: stdin})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("execute command in VM %s timed out", name)
	}
	if err != nil {
		return nil, runtimes.CommandError("execute command in VM "+name, output, err)
	}
	return output, nil
}

func (m *Manager) Copy(ctx context.Context, source, destination string) error {
	if strings.TrimSpace(source) == "" || strings.TrimSpace(destination) == "" {
		return errors.New("VM copy source and destination are required")
	}
	_, err := m.run(ctx, 30*time.Minute, "copy VM file", "copy", source, destination)
	return err
}

func (m *Manager) CreateSnapshot(ctx context.Context, name, snapshot string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return fmt.Errorf("invalid snapshot name: %w", err)
	}
	_, err := m.run(ctx, 10*time.Minute, "create VM snapshot", "snapshot", "create", name, "--name", snapshot)
	return err
}

func (m *Manager) RestoreSnapshot(ctx context.Context, name, snapshot string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return fmt.Errorf("invalid snapshot name: %w", err)
	}
	_, err := m.run(ctx, 10*time.Minute, "restore VM snapshot", "snapshot", "revert", name, "--name", snapshot)
	return err
}

func (m *Manager) DeleteSnapshot(ctx context.Context, name, snapshot string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if err := validateName(snapshot); err != nil {
		return fmt.Errorf("invalid snapshot name: %w", err)
	}
	_, err := m.run(ctx, 10*time.Minute, "delete VM snapshot", "snapshot", "delete", name, "--name", snapshot)
	return err
}

func (m *Manager) action(ctx context.Context, action, name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	_, err := m.run(ctx, 5*time.Minute, action+" Lima instance", action, name)
	return err
}

func (m *Manager) run(ctx context.Context, timeout time.Duration, action string, args ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "limactl", Args: args})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", action, timeout)
	}
	if err != nil {
		return nil, runtimes.CommandError(action, output, err)
	}
	return output, nil
}

func (m *Manager) image(id string) (Image, bool) {
	for _, image := range m.Images() {
		if image.ID == id {
			return image, true
		}
	}
	return Image{}, false
}

func decodeInstance(raw map[string]any) Instance {
	return Instance{
		Name:         stringValue(raw, "name", "Name"),
		Status:       stringValue(raw, "status", "Status"),
		Architecture: stringValue(raw, "arch", "Arch"),
		CPUs:         int(numberValue(raw, "cpus", "CPUs")),
		MemoryBytes:  numberValue(raw, "memory", "Memory"),
		DiskBytes:    numberValue(raw, "disk", "Disk"),
		SSHLocalPort: int(numberValue(raw, "sshLocalPort", "SSHLocalPort")),
		Directory:    stringValue(raw, "dir", "Dir"),
		Addresses:    stringSliceValue(raw, "addresses", "Addresses"),
	}
}

func stringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			if result, ok := value.(string); ok {
				return result
			}
		}
	}
	return ""
}

func numberValue(values map[string]any, keys ...string) int64 {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return int64(value)
		case json.Number:
			result, _ := value.Int64()
			return result
		case string:
			result, _ := strconv.ParseInt(value, 10, 64)
			return result
		}
	}
	return 0
}

func stringSliceValue(values map[string]any, keys ...string) []string {
	for _, key := range keys {
		raw, ok := values[key].([]any)
		if !ok {
			continue
		}
		result := make([]string, 0, len(raw))
		for _, value := range raw {
			if item, ok := value.(string); ok {
				result = append(result, item)
			}
		}
		return result
	}
	return nil
}

func validateName(name string) error {
	if !vmNamePattern.MatchString(name) {
		return fmt.Errorf("VM name must match %s", vmNamePattern)
	}
	return nil
}
