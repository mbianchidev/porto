package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const defaultTimeout = 20 * time.Second

var ErrUnavailable = errors.New("Docker is unavailable")

type Manager struct {
	runner  runtimes.Runner
	timeout time.Duration
}

type Status struct {
	Enabled       bool   `json:"enabled"`
	Available     bool   `json:"available"`
	Context       string `json:"context"`
	Endpoint      string `json:"endpoint"`
	ClientVersion string `json:"clientVersion"`
	ServerVersion string `json:"serverVersion"`
	ProxySocket   string `json:"proxySocket,omitempty"`
	CanonicalPath string `json:"canonicalPath,omitempty"`
	CanonicalLink string `json:"canonicalLink,omitempty"`
	Canonical     bool   `json:"canonical"`
	PreviousLink  string `json:"previousLink,omitempty"`
	Message       string `json:"message,omitempty"`
}

type Container struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Image          string `json:"image"`
	State          string `json:"state"`
	Status         string `json:"status"`
	Ports          string `json:"ports"`
	Networks       string `json:"networks"`
	Mounts         string `json:"mounts"`
	CreatedAt      string `json:"createdAt"`
	ComposeProject string `json:"composeProject,omitempty"`
	ComposeService string `json:"composeService,omitempty"`
}

type Image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
	Size       string `json:"size"`
	CreatedAt  string `json:"createdAt"`
}

type Network struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Scope    string `json:"scope"`
	Internal string `json:"internal"`
	IPv6     string `json:"ipv6"`
	Created  string `json:"createdAt"`
}

type Volume struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Mountpoint string `json:"mountpoint"`
	Scope      string `json:"scope"`
	CreatedAt  string `json:"createdAt"`
}

type Build struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
	Duration  string `json:"duration"`
	Platform  string `json:"platform"`
}

type BuildRequest struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile"`
	Tag        string `json:"tag"`
	Target     string `json:"target"`
	Platform   string `json:"platform"`
	NoCache    bool   `json:"noCache"`
}

type ContainerStats struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CPU      string `json:"cpu"`
	Memory   string `json:"memory"`
	MemoryPC string `json:"memoryPercent"`
	Network  string `json:"network"`
	BlockIO  string `json:"blockIO"`
	PIDs     string `json:"pids"`
}

type CreateNetworkRequest struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Subnet   string `json:"subnet"`
	Gateway  string `json:"gateway"`
	Internal bool   `json:"internal"`
}

func New(runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{runner: runner, timeout: defaultTimeout}
}

func (m *Manager) Status(ctx context.Context, proxySocket string) Status {
	status := Status{ProxySocket: proxySocket}
	contextName, err := m.run(ctx, "inspect Docker context", "context", "show")
	if err != nil {
		status.Message = err.Error()
		return status
	}
	status.Context = strings.TrimSpace(string(contextName))
	endpoint, err := m.Endpoint(ctx)
	if err != nil {
		status.Message = err.Error()
		return status
	}
	status.Endpoint = endpoint
	output, err := m.run(ctx, "inspect Docker version", "version", "--format", "{{json .}}")
	if err != nil {
		status.Message = err.Error()
		return status
	}
	var version struct {
		Client struct {
			Version string `json:"Version"`
		} `json:"Client"`
		Server *struct {
			Version string `json:"Version"`
		} `json:"Server"`
	}
	if err := json.Unmarshal(output, &version); err != nil {
		status.Message = fmt.Sprintf("decode Docker version: %v", err)
		return status
	}
	status.Available = version.Server != nil && version.Server.Version != ""
	status.ClientVersion = version.Client.Version
	if version.Server != nil {
		status.ServerVersion = version.Server.Version
	}
	if !status.Available {
		status.Message = ErrUnavailable.Error()
	}
	return status
}

func (m *Manager) Endpoint(ctx context.Context) (string, error) {
	if endpoint := strings.TrimSpace(os.Getenv("PORTO_DOCKER_UPSTREAM")); endpoint != "" {
		return normalizeEndpoint(endpoint)
	}
	contextName, err := m.run(ctx, "resolve Docker context", "context", "show")
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(contextName))
	if name == "" {
		name = "default"
	}
	output, err := m.run(ctx, "inspect Docker endpoint", "context", "inspect", name, "--format", "{{json .Endpoints.docker.Host}}")
	if err != nil {
		return "", err
	}
	var endpoint string
	if err := json.Unmarshal(output, &endpoint); err != nil {
		return "", fmt.Errorf("decode Docker endpoint: %w", err)
	}
	return normalizeEndpoint(endpoint)
}

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("Docker context has no endpoint")
	}
	const unixPrefix = "unix://"
	if strings.HasPrefix(endpoint, unixPrefix) {
		path := strings.TrimPrefix(endpoint, unixPrefix)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return unixPrefix + path, nil
	}
	return endpoint, nil
}

func (m *Manager) Containers(ctx context.Context) ([]Container, error) {
	output, err := m.run(ctx, "list Docker containers", "ps", "-a", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Container {
		return Container{
			ID:             item["ID"],
			Name:           item["Names"],
			Image:          item["Image"],
			State:          item["State"],
			Status:         item["Status"],
			Ports:          item["Ports"],
			Networks:       item["Networks"],
			Mounts:         item["Mounts"],
			CreatedAt:      item["CreatedAt"],
			ComposeProject: labelValue(item["Labels"], "com.docker.compose.project"),
			ComposeService: labelValue(item["Labels"], "com.docker.compose.service"),
		}
	})
}

func (m *Manager) Images(ctx context.Context) ([]Image, error) {
	output, err := m.run(ctx, "list Docker images", "image", "ls", "--digests", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Image {
		return Image{
			ID:         item["ID"],
			Repository: item["Repository"],
			Tag:        item["Tag"],
			Digest:     item["Digest"],
			Size:       item["Size"],
			CreatedAt:  first(item, "CreatedAt", "CreatedSince"),
		}
	})
}

func (m *Manager) Networks(ctx context.Context) ([]Network, error) {
	output, err := m.run(ctx, "list Docker networks", "network", "ls", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Network {
		return Network{
			ID:       item["ID"],
			Name:     item["Name"],
			Driver:   item["Driver"],
			Scope:    item["Scope"],
			Internal: item["Internal"],
			IPv6:     item["IPv6"],
			Created:  item["CreatedAt"],
		}
	})
}

func (m *Manager) Volumes(ctx context.Context) ([]Volume, error) {
	output, err := m.run(ctx, "list Docker volumes", "volume", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Volume {
		return Volume{
			Name:       item["Name"],
			Driver:     item["Driver"],
			Mountpoint: item["Mountpoint"],
			Scope:      item["Scope"],
			CreatedAt:  item["CreatedAt"],
		}
	})
}

func (m *Manager) Builds(ctx context.Context) ([]Build, error) {
	output, err := m.run(ctx, "list Docker build history", "buildx", "history", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) Build {
		return Build{
			ID:        first(item, "ID", "Ref"),
			Name:      first(item, "Name", "Ref"),
			Status:    item["Status"],
			CreatedAt: first(item, "CreatedAt", "Created"),
			Duration:  item["Duration"],
			Platform:  item["Platform"],
		}
	})
}

func (m *Manager) ContainerAction(ctx context.Context, id, action string) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	var args []string
	switch action {
	case "start", "stop", "restart", "pause", "unpause":
		args = []string{action, id}
	case "remove":
		args = []string{"rm", id}
	case "remove-force":
		args = []string{"rm", "--force", id}
	default:
		return fmt.Errorf("unsupported container action %q", action)
	}
	_, err := m.run(ctx, action+" Docker container", args...)
	return err
}

func (m *Manager) InspectContainer(ctx context.Context, id string) (json.RawMessage, error) {
	return m.inspect(ctx, "container", id)
}

func (m *Manager) ContainerLogs(ctx context.Context, id string, tail int) ([]byte, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	if tail <= 0 || tail > 10000 {
		tail = 500
	}
	return m.run(ctx, "read Docker container logs", "logs", "--timestamps", "--tail", strconv.Itoa(tail), id)
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
	commandContext, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "docker", Args: args, Stdin: stdin})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, errors.New("Docker container command timed out")
	}
	if err != nil {
		return nil, runtimes.CommandError("execute Docker container command", output, err)
	}
	return output, nil
}

func (m *Manager) ContainerStats(ctx context.Context) ([]ContainerStats, error) {
	output, err := m.run(ctx, "read Docker container stats", "stats", "--no-stream", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return decodeLines(output, func(item map[string]string) ContainerStats {
		return ContainerStats{
			ID:       item["ID"],
			Name:     item["Name"],
			CPU:      item["CPUPerc"],
			Memory:   item["MemUsage"],
			MemoryPC: item["MemPerc"],
			Network:  item["NetIO"],
			BlockIO:  item["BlockIO"],
			PIDs:     item["PIDs"],
		}
	})
}

func (m *Manager) InspectImage(ctx context.Context, id string) (json.RawMessage, error) {
	return m.inspect(ctx, "image", id)
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
	args = append(args, id)
	_, err := m.run(ctx, "remove Docker image", args...)
	return err
}

func (m *Manager) PullImage(ctx context.Context, reference string) error {
	if err := validateObjectID(reference); err != nil {
		return err
	}
	_, err := m.run(ctx, "pull Docker image", "pull", reference)
	return err
}

func (m *Manager) Build(ctx context.Context, request BuildRequest) ([]byte, error) {
	if strings.TrimSpace(request.Context) == "" {
		return nil, errors.New("build context is required")
	}
	args := []string{"build"}
	if request.Dockerfile != "" {
		args = append(args, "--file", request.Dockerfile)
	}
	if request.Tag != "" {
		args = append(args, "--tag", request.Tag)
	}
	if request.Target != "" {
		args = append(args, "--target", request.Target)
	}
	if request.Platform != "" {
		args = append(args, "--platform", request.Platform)
	}
	if request.NoCache {
		args = append(args, "--no-cache")
	}
	args = append(args, request.Context)
	return m.runWithTimeout(ctx, 30*time.Minute, "build Docker image", args...)
}

func (m *Manager) CreateNetwork(ctx context.Context, request CreateNetworkRequest) error {
	if err := validateObjectID(request.Name); err != nil {
		return err
	}
	args := []string{"network", "create"}
	if request.Driver != "" {
		args = append(args, "--driver", request.Driver)
	}
	if request.Subnet != "" {
		args = append(args, "--subnet", request.Subnet)
	}
	if request.Gateway != "" {
		args = append(args, "--gateway", request.Gateway)
	}
	if request.Internal {
		args = append(args, "--internal")
	}
	args = append(args, request.Name)
	_, err := m.run(ctx, "create Docker network", args...)
	return err
}

func (m *Manager) RemoveNetwork(ctx context.Context, name string) error {
	if err := validateObjectID(name); err != nil {
		return err
	}
	_, err := m.run(ctx, "remove Docker network", "network", "rm", name)
	return err
}

func (m *Manager) CreateVolume(ctx context.Context, name, driver string) error {
	if err := validateObjectID(name); err != nil {
		return err
	}
	args := []string{"volume", "create"}
	if driver != "" {
		args = append(args, "--driver", driver)
	}
	args = append(args, name)
	_, err := m.run(ctx, "create Docker volume", args...)
	return err
}

func (m *Manager) RemoveVolume(ctx context.Context, name string, force bool) error {
	if err := validateObjectID(name); err != nil {
		return err
	}
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, err := m.run(ctx, "remove Docker volume", args...)
	return err
}

func (m *Manager) InstallContext(ctx context.Context, socketPath string) error {
	endpoint := "unix://" + socketPath
	if strings.HasPrefix(socketPath, `\\.\pipe\`) {
		endpoint = "npipe:////./pipe/" + strings.TrimPrefix(socketPath, `\\.\pipe\`)
	}
	if _, err := m.run(ctx, "inspect Porto Docker context", "context", "inspect", "porto"); err == nil {
		_, err = m.run(ctx, "update Porto Docker context", "context", "update", "porto", "--docker", "host="+endpoint)
		return err
	}
	_, err := m.run(ctx, "create Porto Docker context", "context", "create", "porto", "--docker", "host="+endpoint)
	return err
}

func (m *Manager) inspect(ctx context.Context, kind, id string) (json.RawMessage, error) {
	if err := validateObjectID(id); err != nil {
		return nil, err
	}
	output, err := m.run(ctx, "inspect Docker "+kind, kind, "inspect", id)
	if err != nil {
		return nil, err
	}
	if !json.Valid(output) {
		return nil, fmt.Errorf("Docker %s inspect returned invalid JSON", kind)
	}
	return json.RawMessage(output), nil
}

func (m *Manager) run(ctx context.Context, action string, args ...string) ([]byte, error) {
	return m.runWithTimeout(ctx, m.timeout, action, args...)
}

func (m *Manager) runWithTimeout(ctx context.Context, timeout time.Duration, action string, args ...string) ([]byte, error) {
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "docker", Args: args})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", action, timeout)
	}
	if err != nil {
		return nil, runtimes.CommandError(action, output, err)
	}
	return output, nil
}

func decodeLines[T any](output []byte, convert func(map[string]string) T) ([]T, error) {
	items := make([]T, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("decode Docker output line %q: %w", line, err)
		}
		item := make(map[string]string, len(raw))
		for key, value := range raw {
			switch typed := value.(type) {
			case string:
				item[key] = typed
			case float64:
				item[key] = strconv.FormatFloat(typed, 'f', -1, 64)
			case bool:
				item[key] = strconv.FormatBool(typed)
			default:
				encoded, _ := json.Marshal(typed)
				item[key] = string(encoded)
			}
		}
		items = append(items, convert(item))
	}
	return items, scanner.Err()
}

func first(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if values[key] != "" {
			return values[key]
		}
	}
	return ""
}

func validateObjectID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("Docker object identifier is required")
	}
	if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid Docker object identifier %q", value)
	}
	return nil
}

func labelValue(labels, name string) string {
	for _, label := range strings.Split(labels, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(label), "=")
		if ok && key == name {
			return value
		}
	}
	return ""
}
