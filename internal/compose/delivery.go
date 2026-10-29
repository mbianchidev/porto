package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/operations"
	"github.com/mbianchidev/porto/internal/runtimes"
	"gopkg.in/yaml.v3"
)

func (i *Integration) SetDefinitionResolver(resolve func(app.Project) (string, error)) {
	i.resolveDefinition = resolve
}

func (i *Integration) configFile(project app.Project) (string, error) {
	if i.resolveDefinition != nil {
		path, err := i.resolveDefinition(project)
		if err != nil {
			return "", err
		}
		if path != "" {
			return path, nil
		}
	}
	return configFile(project)
}

func composeArguments(project app.Project, file string) []string {
	args := []string{"compose"}
	if filepath.IsAbs(file) {
		args = append(args, "--project-directory", project.Path)
	}
	return append(args, "-f", file)
}

func UpArguments(project app.Project, plan PortPlan, reconcile bool) []string {
	args := []string{"compose", "--project-directory", project.Path, "-p", ProjectName(project), "-f", plan.ConfigFile}
	if plan.OverridePath != "" {
		args = append(args, "-f", plan.OverridePath)
	}
	args = append(args, "up")
	if reconcile {
		args = append(args, "--remove-orphans")
	}
	return args
}

func (i *Integration) ValidateDefinition(ctx context.Context, project app.Project, directory string, definition []byte, variables map[string]string) ([]byte, error) {
	root, err := config.RuntimeDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "compose"), 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(filepath.Join(root, "compose"), ".validate-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	path := filepath.Join(work, "compose.yaml")
	if err := config.WriteFileAtomic(path, definition, 0o600); err != nil {
		return nil, err
	}
	dockerConfig, err := isolatedPluginConfig(work)
	if err != nil {
		return nil, err
	}
	environment := localtools.IsolatedEnvironment(work, "COMPOSE_PROFILES=", "COMPOSE_ENV_FILES=")
	for name, value := range variables {
		environment = append(environment, name+"="+value)
	}
	environment = append(environment, i.dockerEnvironment("COMPOSE_PROJECT_NAME="+ProjectName(project), "DOCKER_CONFIG="+dockerConfig)...)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := (localtools.ExecRunner{}).Run(ctx, runtimes.Command{Name: "docker", Dir: directory, Args: []string{"compose", "--project-directory", directory, "-p", ProjectName(project), "-f", path, "config", "--format", "json"}, Env: environment})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, operations.Failure("Compose validation failed. Check YAML, required variables, referenced files and the installed Compose client; no definition was applied.")
	}
	var document struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal(output.Stdout, &document) != nil || len(document.Services) == 0 {
		return nil, operations.Failure("Compose definition has no valid services.")
	}
	for _, service := range document.Services {
		if service.Image != "" {
			if _, err := reference.ParseAnyReference(service.Image); err != nil {
				return nil, operations.Failure("Compose definition has an invalid image reference; use a credential-free image name, tag or digest.")
			}
		}
	}
	return output.Stdout, nil
}

func (i *Integration) WaitHealthy(ctx context.Context, project app.Project) error {
	file, err := i.configFile(project)
	if err != nil {
		return err
	}
	base := composeArguments(project, file)
	override, err := OverridePath(project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(override); err == nil {
		base = append(base, "-f", override)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	environment := i.dockerEnvironment("COMPOSE_PROJECT_NAME="+ProjectName(project), "COMPOSE_PROFILES=")
	run := func(arguments ...string) ([]byte, error) {
		probe, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return i.runner.Run(probe, Command{Dir: project.Path, Name: "docker", Args: append(slices.Clone(base), arguments...), Env: environment})
	}
	output, err := run("config", "--format", "json")
	if err != nil {
		return operations.Failure("Unable to inspect effective Compose services while checking deployment health.")
	}
	var document struct {
		Services map[string]serviceConfiguration `json:"services"`
	}
	if json.Unmarshal(output, &document) != nil {
		return operations.Failure("Compose returned invalid service configuration during readiness checks.")
	}
	expected := map[string]int{}
	for name, service := range document.Services {
		if len(service.Profiles) == 0 {
			replicas := service.desiredReplicas()
			if replicas < 0 {
				return operations.Failure("Compose returned an invalid service replica count during readiness checks.")
			}
			expected[name] = replicas
		}
	}
	if len(expected) == 0 {
		return operations.Failure("Compose definition has no services enabled in the selected profile.")
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := run("ps", "--all", "--format", "json")
		if err != nil {
			return operations.Failure("Unable to inspect Compose service health; check the selected project's containers.")
		}
		processes, err := decodeDeliveryProcesses(output)
		if err != nil {
			return err
		}
		ready := map[string]int{}
		scalingDown := false
		for _, process := range processes {
			replicas, tracked := expected[process.Service]
			if !tracked {
				continue
			}
			if replicas == 0 {
				if process.State != "exited" && process.State != "dead" {
					scalingDown = true
				}
				continue
			}
			if process.Health == "unhealthy" || process.State == "dead" || process.State == "exited" && process.ExitCode != 0 {
				return operations.Failure("A selected Compose service failed or is unhealthy. Inspect its container health and redacted project logs before retrying.")
			}
			if process.State == "running" && (process.Health == "" || process.Health == "healthy") || process.State == "exited" && process.ExitCode == 0 {
				ready[process.Service]++
			}
		}
		complete := !scalingDown
		for service, replicas := range expected {
			if ready[service] != replicas {
				complete = false
			}
		}
		if complete {
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type deliveryProcess struct {
	Service, State, Health string
	ExitCode               int
}

func decodeDeliveryProcesses(output []byte) ([]deliveryProcess, error) {
	result := []deliveryProcess{}
	text := strings.TrimSpace(string(output))
	if text == "" {
		return result, nil
	}
	if strings.HasPrefix(text, "[") {
		if json.Unmarshal(output, &result) != nil {
			return nil, operations.Failure("Compose returned malformed deployment service state.")
		}
		return result, nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var value deliveryProcess
		if json.Unmarshal([]byte(line), &value) != nil {
			return nil, operations.Failure("Compose returned malformed deployment service state.")
		}
		result = append(result, value)
	}
	return result, nil
}

func priorPorts(project app.Project) (map[string]int, error) {
	result := map[string]int{}
	path, err := OverridePath(project)
	if err != nil {
		return result, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var configuration struct {
		Services map[string]struct {
			Ports []string `yaml:"ports"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(data, &configuration) != nil {
		return nil, errors.New("stored Compose port override is invalid")
	}
	for service, value := range configuration.Services {
		for _, port := range value.Ports {
			parts := strings.Split(port, ":")
			if len(parts) != 3 {
				return nil, errors.New("stored Compose port override is not loopback scoped")
			}
			published, err := strconv.Atoi(parts[1])
			if err != nil || published < 1 || published > 65535 {
				return nil, errors.New("stored Compose port is invalid")
			}
			result[fmt.Sprintf("%s:%s", service, parts[2])] = published
		}
	}
	return result, nil
}
