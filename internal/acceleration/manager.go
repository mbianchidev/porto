package acceleration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type Manager struct {
	root     string
	runner   runtimes.Runner
	lookPath func(string) (string, error)
	goos     string
	goarch   string
	native   func() (Capability, Capability)
}

func New(root string, runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{root: root, runner: runner, lookPath: runtimes.LookPath,
		goos: runtime.GOOS, goarch: runtime.GOARCH, native: nativeCapabilities}
}

func (m *Manager) Settings() (Settings, error) {
	settings := Defaults()
	if m.root == "" {
		return settings, nil
	}
	data, err := os.ReadFile(filepath.Join(m.root, "acceleration.json"))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("read architecture policy: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return settings, fmt.Errorf("decode architecture policy: %w", err)
	}
	return settings, Validate(settings)
}

func (m *Manager) Save(ctx context.Context, settings Settings) (err error) {
	if err := Validate(settings); err != nil {
		return err
	}
	if m.root == "" {
		return errors.New("architecture settings storage is unavailable")
	}
	if settings.RosettaEnabled {
		host := m.ProbeHost(ctx)
		if !host.Rosetta.Supported {
			return fmt.Errorf("Rosetta is not available; install it explicitly with 'softwareupdate --install-rosetta', then refresh capabilities: %s", host.Rosetta.Reason)
		}
	}
	lease, err := filelock.Acquire(ctx, filepath.Join(m.root, "acceleration.lock"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "acceleration.json"), append(data, '\n'), 0o600)
}

func (m *Manager) ProbeHost(ctx context.Context) Capabilities {
	result := Capabilities{HostOS: m.goos, HostArchitecture: m.goarch}
	result.Virtualization, result.Rosetta = m.native()
	if path, err := m.lookPath("limactl"); err == nil {
		output, err := m.probeCommand(ctx, path, "info")
		if err != nil {
			result.Virtualization = Capability{Reason: err.Error()}
			result.Rosetta = Capability{Reason: err.Error()}
		} else {
			var info struct {
				Version     string                     `json:"version"`
				VMTypes     []string                   `json:"vmTypes"`
				GuestAgents map[string]json.RawMessage `json:"guestAgents"`
			}
			if err := json.Unmarshal(output, &info); err != nil {
				result.Virtualization = Capability{Reason: "decode Lima capabilities: " + err.Error()}
				result.Rosetta = result.Virtualization
			} else {
				result.LimaVersion = info.Version
				for architecture := range info.GuestAgents {
					if normalized := Architecture(architecture); normalized == "amd64" || normalized == "arm64" {
						result.GuestArchitectures = append(result.GuestArchitectures, normalized)
					}
				}
				if !slices.Contains(info.VMTypes, "vz") {
					result.Virtualization = Capability{Reason: "this Lima build does not provide the VZ driver"}
					result.Rosetta = result.Virtualization
				}
			}
		}
	} else {
		result.Virtualization = Capability{Reason: "Lima is missing; install limactl and refresh capabilities"}
		result.Rosetta = result.Virtualization
	}
	for _, target := range []struct{ architecture, command string }{
		{"amd64", "qemu-system-x86_64"}, {"arm64", "qemu-system-aarch64"},
	} {
		if path, err := m.lookPath(target.command); err == nil {
			output, err := m.probeCommand(ctx, path, "--version")
			if err == nil {
				result.QEMUVersion = strings.TrimSpace(string(output))
				result.QEMUArchitectures = append(result.QEMUArchitectures, target.architecture)
			}
		}
	}
	result.RosettaHost = result.Rosetta
	return result
}

func (m *Manager) probeCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner.Run(ctx, runtimes.Command{Name: name, Args: args})
	if err != nil {
		return nil, runtimes.CommandError("probe architecture capabilities", output, err)
	}
	return output, nil
}
