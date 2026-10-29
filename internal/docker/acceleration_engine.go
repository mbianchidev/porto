package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mbianchidev/porto/internal/acceleration"
	"gopkg.in/yaml.v3"
)

const selectAMD64HandlerScript = `set -eu
mode="$1"
root=/proc/sys/fs/binfmt_misc
if [ "$mode" = rosetta ]; then
  test -r "$root/rosetta" && grep -qx enabled "$root/rosetta" && grep -q '^flags: .*F' "$root/rosetta" || {
    echo "Rosetta's container-safe handler is unavailable after engine restart" >&2
    exit 1
  }
  interpreter="$(sed -n 's/^interpreter //p' "$root/rosetta")"
  test -x "$interpreter"
  if [ -e "$root/qemu-x86_64" ]; then printf '%s\n' -1 > "$root/qemu-x86_64"; fi
else
  if [ -e "$root/rosetta" ]; then printf '%s\n' -1 > "$root/rosetta"; fi
  if [ "$(uname -m)" = aarch64 ]; then
    if [ ! -r "$root/qemu-x86_64" ]; then systemctl restart systemd-binfmt.service; fi
    if [ -e "$root/rosetta" ]; then printf '%s\n' -1 > "$root/rosetta"; fi
    test -r "$root/qemu-x86_64" && grep -qx enabled "$root/qemu-x86_64" && grep -q '^flags: .*F' "$root/qemu-x86_64" || {
      echo "The owned engine's container-safe QEMU amd64 handler is unavailable" >&2
      exit 1
    }
  fi
fi
`

func (m *Manager) accelerationCreateArguments(ctx context.Context) ([]string, error) {
	if m.architecture == nil {
		return nil, nil
	}
	settings, err := m.architecture.Settings()
	if err != nil || !settings.RosettaEnabled {
		return nil, err
	}
	host := m.architecture.ProbeHost(ctx)
	if !host.Rosetta.Supported {
		return nil, fmt.Errorf("Rosetta must already be explicitly installed before creating the engine: %s", host.Rosetta.Reason)
	}
	return []string{"--vm-type=vz", "--arch=aarch64",
		"--set=.vmOpts.vz.rosetta.enabled = true | .vmOpts.vz.rosetta.binfmt = true"}, nil
}

func (m *Manager) configureAMD64Handler(ctx context.Context) error {
	if m.architecture == nil {
		return nil
	}
	settings, err := m.architecture.Settings()
	if err != nil {
		return err
	}
	mode := "qemu"
	if settings.RosettaEnabled {
		mode = "rosetta"
	}
	_, err = m.runCommand(ctx, time.Minute, "select owned engine amd64 translator", nil,
		"limactl", "shell", "--workdir=/", engineInstanceName, "--",
		"sudo", "-n", "sh", "-c", selectAMD64HandlerScript, "porto-amd64-handler", mode)
	return err
}

func (m *Manager) ValidateAccelerationConfiguration(ctx context.Context, settings acceleration.Settings) error {
	if err := acceleration.Validate(settings); err != nil {
		return err
	}
	if !settings.RosettaEnabled {
		return nil
	}
	state, err := m.readEngineState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if state.Mode != "lima" {
		return fmt.Errorf("%w: Rosetta cannot be installed into an external container engine", ErrUnsupported)
	}
	configuration, err := m.engineConfiguration(ctx, state.Instance)
	if err != nil {
		return err
	}
	if configuration.VMType != "vz" || acceleration.Architecture(configuration.Arch) != "arm64" {
		return errors.New("Rosetta requires an existing arm64 VZ engine; Porto will not change VM drivers or replace a QEMU engine disk")
	}
	return nil
}

func (m *Manager) ApplyAcceleration(ctx context.Context, restartConfirmed bool) error {
	if m.architecture == nil {
		return errors.New("architecture settings are unavailable")
	}
	settings, err := m.architecture.Settings()
	if err != nil {
		return err
	}
	if err := m.ValidateAccelerationConfiguration(ctx, settings); err != nil {
		return err
	}
	if settings.RosettaEnabled {
		host := m.architecture.ProbeHost(ctx)
		if !host.Rosetta.Supported {
			return fmt.Errorf("Rosetta host capability changed: %s", host.Rosetta.Reason)
		}
	}
	return m.editEngineConfiguration(ctx, restartConfirmed, func(configuration EngineConfiguration, original []byte) (string, error) {
		if settings.RosettaEnabled && (configuration.VMType != "vz" || acceleration.Architecture(configuration.Arch) != "arm64") {
			return "", errors.New("Rosetta requires an arm64 VZ engine; driver conversion and disk replacement are not supported")
		}
		return accelerationEngineExpression(settings, original)
	}, nil, func(ctx context.Context) error {
		if err := m.configureAMD64Handler(ctx); err != nil {
			return err
		}
		state, err := m.readEngineState()
		if err != nil {
			return err
		}
		return m.refreshLimaBuildKitPlatforms(ctx, state.OwnerID)
	})
}

func accelerationEngineExpression(settings acceleration.Settings, original []byte) (string, error) {
	var configuration struct {
		Rosetta struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"rosetta"`
		VMOpts struct {
			VZ struct {
				Rosetta struct {
					Enabled bool `yaml:"enabled"`
				} `yaml:"rosetta"`
			} `yaml:"vz"`
		} `yaml:"vmOpts"`
	}
	if err := yaml.Unmarshal(original, &configuration); err != nil {
		return "", fmt.Errorf("read existing VZ device configuration: %w", err)
	}
	shareEnabled := settings.RosettaEnabled || configuration.Rosetta.Enabled || configuration.VMOpts.VZ.Rosetta.Enabled
	return fmt.Sprintf(".vmOpts.vz.rosetta.enabled = %t | .vmOpts.vz.rosetta.binfmt = %t", shareEnabled, settings.RosettaEnabled), nil
}
