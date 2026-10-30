package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func NormalizeProfiles(profiles []string) ([]string, error) {
	if len(profiles) > 64 {
		return nil, errors.New("select at most 64 Compose profiles")
	}
	result := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		if profile != "*" && !profileName.MatchString(profile) {
			return nil, errors.New("Compose profiles must be names of up to 128 letters, digits, dots, underscores or hyphens, beginning with a letter or digit, or *")
		}
		result = append(result, profile)
	}
	slices.Sort(result)
	result = slices.Compact(result)
	if slices.Contains(result, "*") {
		return []string{"*"}, nil
	}
	return result, nil
}

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

func (i *Integration) ValidateDefinition(ctx context.Context, project app.Project, directory string, definition []byte, variables map[string]string, profiles ...string) ([]byte, error) {
	profiles, err := NormalizeProfiles(profiles)
	if err != nil {
		return nil, err
	}
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
	arguments := []string{"compose", "--project-directory", directory, "-p", ProjectName(project), "-f", path}
	for _, profile := range profiles {
		arguments = append(arguments, "--profile", profile)
	}
	run := func(options ...string) (localtools.Output, error) {
		return (localtools.ExecRunner{}).Run(ctx, runtimes.Command{Name: "docker", Dir: directory, Args: append(slices.Clone(arguments), options...), Env: environment})
	}
	if len(profiles) > 0 && !slices.Contains(profiles, "*") {
		catalog, err := run("config", "--profiles")
		if err != nil {
			return nil, operations.Failure("Unable to discover declared Compose profiles. Check YAML, required variables and referenced files; no definition was applied.")
		}
		available := strings.Fields(string(catalog.Stdout))
		for _, profile := range profiles {
			if !slices.Contains(available, profile) {
				return nil, operations.Failure("A selected Compose profile is not declared by this definition. Check the selected profile names; no definition was applied.")
			}
		}
	}
	output, err := run("config", "--format", "json")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, operations.Failure("Compose validation failed. Check YAML, required variables, referenced files and the installed Compose client; no definition was applied.")
	}
	var document map[string]json.RawMessage
	var services map[string]map[string]json.RawMessage
	if json.Unmarshal(output.Stdout, &document) != nil || json.Unmarshal(document["services"], &services) != nil || len(services) == 0 {
		return nil, operations.Failure("Compose definition has no valid services.")
	}
	for _, service := range services {
		if service == nil {
			return nil, operations.Failure("Compose returned an invalid effective service.")
		}
		var image string
		if encoded, found := service["image"]; found {
			if err := json.Unmarshal(encoded, &image); err != nil {
				return nil, operations.Failure("Compose definition has an invalid image reference.")
			}
		}
		if image != "" {
			if _, err := reference.ParseAnyReference(image); err != nil {
				return nil, operations.Failure("Compose definition has an invalid image reference; use a credential-free image name, tag or digest.")
			}
		}
		// Native config already selected the active services. Freeze that set for later starts and rollbacks.
		delete(service, "profiles")
	}
	document["services"], err = json.Marshal(services)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
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
	completionJobs := map[string]bool{}
	for name, service := range document.Services {
		if len(service.Profiles) == 0 {
			replicas := service.desiredReplicas()
			if replicas < 0 {
				return operations.Failure("Compose returned an invalid service replica count during readiness checks.")
			}
			expected[name] = replicas
			if replicas > 0 {
				for dependency, requirement := range service.DependsOn {
					if requirement.Condition == "service_completed_successfully" {
						completionJobs[dependency] = true
					}
				}
			}
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
			if process.Health == "unhealthy" || process.State == "dead" {
				return operations.Failure("A selected Compose service failed or is unhealthy. Inspect its container health and redacted project logs before retrying.")
			}
			if process.State == "exited" {
				if !completionJobs[process.Service] || process.ExitCode == nil || *process.ExitCode != 0 {
					return operations.Failure("A selected Compose service exited without satisfying its declared readiness. Only an explicit service_completed_successfully dependency with an observed zero exit code counts as a completed job.")
				}
				ready[process.Service]++
			} else if process.State == "running" && !completionJobs[process.Service] &&
				(process.Health == "healthy" || process.Health == "" && !document.Services[process.Service].requiresHealth()) {
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
	ExitCode               *int
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
