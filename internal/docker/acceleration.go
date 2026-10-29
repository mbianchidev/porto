package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/platforms"
	"github.com/mbianchidev/porto/internal/acceleration"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/solver/pb"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/proto"
)

func (m *Manager) SetAccelerationManager(manager *acceleration.Manager) {
	m.architecture = manager
}

func (m *Manager) AccelerationCapabilities(ctx context.Context) (acceleration.Capabilities, error) {
	if m.architecture == nil {
		return acceleration.Capabilities{}, errors.New("architecture capability service is not initialized")
	}
	host := m.architecture.ProbeHost(ctx)
	backend, err := m.backendForCommand(ctx)
	if err != nil {
		return host, err
	}
	command := runtimes.Command{Name: "sh", Args: []string{"-c", acceleration.GuestProbeScript}, StdoutOnly: true}
	if backend.limaInstance != "" {
		output, err := m.runStdoutCommand(ctx, 20*time.Second, "inspect engine architecture",
			"limactl", "list", backend.limaInstance, "--json")
		if err != nil {
			return host, err
		}
		var instance struct {
			VMType string `json:"vmType"`
		}
		if err := json.Unmarshal(output, &instance); err != nil {
			return host, fmt.Errorf("decode engine architecture: %w", err)
		}
		host.VMType = instance.VMType
		command.Name = backend.name
		command.Args = []string{"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c", acceleration.GuestProbeScript}
	}
	probeContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	output, err := m.runner.Run(probeContext, command)
	if err != nil {
		return host, runtimes.CommandError("probe backend architecture", output, err)
	}
	return acceleration.WithGuestProbe(host, output)
}

func (m *Manager) selectArchitecture(ctx context.Context, platform, override string) (acceleration.Selection, error) {
	if m.architecture == nil {
		return acceleration.Selection{Platform: platform}, nil
	}
	settings, err := m.architecture.Settings()
	if err != nil {
		return acceleration.Selection{}, err
	}
	policy, err := acceleration.EffectivePolicy(settings.Policy, override)
	if err != nil {
		return acceleration.Selection{}, err
	}
	capabilities, err := m.AccelerationCapabilities(ctx)
	if err != nil {
		return acceleration.Selection{}, err
	}
	return acceleration.Select(capabilities, policy, platform)
}

func (m *Manager) prepareContainerArchitecture(ctx context.Context, request *CreateContainerRequest) error {
	if m.architecture == nil || m.IsNativeWindows() {
		return nil
	}
	override, err := acceleration.EffectivePolicy("", request.Labels[acceleration.PolicyLabel], request.ArchitecturePolicy)
	if err != nil {
		return err
	}
	if request.ArchitecturePolicy == "" && request.Labels[acceleration.PolicyLabel] == "" {
		override = ""
	}
	selection, err := m.selectArchitecture(ctx, request.Platform, override)
	if err != nil {
		return err
	}
	request.Platform = selection.Platform
	request.Labels = cloneStringMap(request.Labels)
	if request.Labels == nil {
		request.Labels = map[string]string{}
	}
	if override != "" {
		request.Labels[acceleration.PolicyLabel] = override
	}
	request.Labels[acceleration.ExecutionLabel] = selection.Execution
	request.Labels[acceleration.PlatformLabel] = selection.Platform
	return nil
}

func (m *Manager) validateExistingArchitecture(ctx context.Context, id string) (acceleration.Selection, error) {
	if m.architecture == nil || m.IsNativeWindows() {
		return acceleration.Selection{}, nil
	}
	document, err := m.InspectContainer(ctx, id)
	if err != nil {
		return acceleration.Selection{}, err
	}
	var container struct {
		Image  string
		Config struct {
			Image  string
			Labels map[string]string
		}
	}
	if err := json.Unmarshal(document, &container); err != nil {
		return acceleration.Selection{}, fmt.Errorf("decode container architecture: %w", err)
	}
	platform := container.Config.Labels[acceleration.PlatformLabel]
	if platform == "" {
		reference := firstNonEmpty(container.Config.Image, container.Image)
		image, err := m.InspectImage(ctx, reference, "")
		if err != nil {
			return acceleration.Selection{}, err
		}
		var configuration struct{ Architecture, Os, Variant string }
		if err := json.Unmarshal(image, &configuration); err != nil {
			return acceleration.Selection{}, fmt.Errorf("decode existing image architecture: %w", err)
		}
		if configuration.Architecture == "" || configuration.Os != "linux" {
			return acceleration.Selection{}, errors.New("existing container image architecture is unavailable; refusing to guess its translation policy")
		}
		platform = platforms.Format(ocispec.Platform{OS: configuration.Os, Architecture: configuration.Architecture, Variant: configuration.Variant})
	}
	return m.selectArchitecture(ctx, platform, container.Config.Labels[acceleration.PolicyLabel])
}

func (m *Manager) recordContainerExecution(ctx context.Context, id string, selection acceleration.Selection) (err error) {
	if selection.Execution == "" {
		return nil
	}
	updates := map[string]string{
		acceleration.ExecutionLabel: selection.Execution,
		acceleration.PlatformLabel:  selection.Platform,
	}
	if handled, operationErr := m.withContainerOperations(ctx, func(operations containerOperations) error {
		return operations.UpdateLabels(ctx, id, updates)
	}); handled {
		if operationErr == nil {
			m.invalidateContainerInventory()
		}
		return operationErr
	}
	backend, close, err := m.resourceSaverRuntime(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, close()) }()
	resolved, err := m.resolveContainerID(ctx, id)
	if err != nil {
		return err
	}
	container, err := backend.client.LoadContainer(withContainerdNamespace(ctx, backend.namespace), resolved)
	if err != nil {
		return err
	}
	_, err = container.SetLabels(withContainerdNamespace(ctx, backend.namespace), updates)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}

func (m *Manager) validateArchitecturePlatforms(ctx context.Context, values, override string) error {
	if m.architecture == nil {
		return nil
	}
	settings, err := m.architecture.Settings()
	if err != nil {
		return err
	}
	policy, err := acceleration.EffectivePolicy(settings.Policy, override)
	if err != nil {
		return err
	}
	capabilities, err := m.AccelerationCapabilities(ctx)
	if err != nil {
		return err
	}
	for _, platform := range strings.Split(values, ",") {
		if _, err := acceleration.Select(capabilities, policy, strings.TrimSpace(platform)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) validateBuildKitArchitecture(ctx context.Context, request *controlapi.SolveRequest) error {
	if m.architecture == nil {
		return nil
	}
	override := request.FrontendAttrs["porto.architecture-policy"]
	requested := []string{request.FrontendAttrs["platform"]}
	for _, encoded := range request.Definition.GetDef() {
		var operation pb.Op
		if err := proto.Unmarshal(encoded, &operation); err != nil {
			return fmt.Errorf("decode BuildKit platform constraints: %w", err)
		}
		if platform := operation.Platform; platform != nil {
			value := platforms.Format(ocispec.Platform{OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant})
			requested = append(requested, value)
		}
	}
	return m.validateArchitecturePlatforms(ctx, strings.Join(requested, ","), override)
}

func (m *Manager) filterBuildKitArchitecture(ctx context.Context, response *controlapi.ListWorkersResponse) error {
	if m.architecture == nil {
		return nil
	}
	settings, err := m.architecture.Settings()
	if err != nil {
		return err
	}
	if settings.Policy != acceleration.NativeOnly {
		return nil
	}
	capabilities, err := m.AccelerationCapabilities(ctx)
	if err != nil {
		return err
	}
	for _, worker := range response.Record {
		allowed := worker.Platforms[:0]
		for _, platform := range worker.Platforms {
			if platform != nil && platform.OS == "linux" && acceleration.Architecture(platform.Architecture) == capabilities.GuestArchitecture {
				allowed = append(allowed, platform)
			}
		}
		worker.Platforms = allowed
	}
	return nil
}
