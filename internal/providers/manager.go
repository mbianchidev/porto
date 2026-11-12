package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/mbianchidev/porto/internal/runtimes"
)

const AppleContainerMinimumVersion = "1.7.1"

func ValidateAppleContainerVersion(output string) error {
	versionText, recognized := strings.CutPrefix(strings.TrimSpace(output), "container CLI version ")
	fields := strings.Fields(versionText)
	if recognized && len(fields) > 0 {
		version, err := semver.Parse(fields[0])
		if err == nil && len(version.Pre) == 0 && version.GTE(semver.Version{Major: 1, Minor: 7, Patch: 1}) {
			return nil
		}
	}
	return fmt.Errorf("Apple Cluster requires stable container %s or newer; install or update it with 'brew install container', then restart Porto", AppleContainerMinimumVersion)
}

type Manager struct {
	runner   runtimes.Runner
	lookPath func(string) (string, error)
	getenv   func(string) string
	goos     string
	goarch   string
}

type Status struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	Message   string `json:"message,omitempty"`
}

type tool struct {
	name    string
	command string
	args    []string
	formula string
}

var tools = []tool{
	{name: "lima", command: "limactl", args: []string{"--version"}, formula: "lima"},
	{name: "qemu", command: qemuCommand(), args: []string{"--version"}, formula: "qemu"},
	{name: "kind", command: "kind", args: []string{"version"}, formula: "kind"},
	{name: "k9s", command: "k9s", args: []string{"version", "--short"}, formula: "k9s"},
	{name: "k0s", command: "limactl", args: []string{"--version"}, formula: "lima"},
	{name: "apple", command: "container", args: []string{"--version"}, formula: "container"},
}

func qemuCommand() string {
	if runtime.GOARCH == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

func New(runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{runner: runner, lookPath: runtimes.LookPath, getenv: os.Getenv, goos: runtime.GOOS, goarch: runtime.GOARCH}
}

func (m *Manager) Status(ctx context.Context) []Status {
	statuses := make([]Status, 0, len(tools))
	for _, candidate := range tools {
		if candidate.name == "apple" && (m.goos != "darwin" || m.goarch != "arm64") {
			continue
		}
		statuses = append(statuses, m.toolStatus(ctx, candidate))
	}
	return statuses
}

func (m *Manager) Install(ctx context.Context, name string) (Status, error) {
	candidate, ok := findTool(name)
	if !ok {
		return Status{}, fmt.Errorf("unknown runtime provider %q", name)
	}
	if candidate.name == "apple" && (m.goos != "darwin" || m.goarch != "arm64") {
		return Status{}, errors.New("Apple Cluster is available only on Apple silicon Macs with macOS 26 or newer")
	}
	current := m.toolStatus(ctx, candidate)
	if current.Installed {
		return current, nil
	}
	if candidate.name == "qemu" && m.goos == "darwin" {
		return current, errors.New(current.Message)
	}
	if m.goos != "darwin" {
		return current, fmt.Errorf("automatic %s installation is currently supported on macOS; install %s and retry", name, candidate.command)
	}
	brewPath, err := m.lookPath("brew")
	if err != nil {
		return current, errors.New("Homebrew is required for automatic provider installation on macOS")
	}
	installContext, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	output, err := m.runner.Run(installContext, runtimes.Command{
		Name: brewPath,
		Args: []string{"install", candidate.formula},
	})
	if err != nil {
		return current, runtimes.CommandError("install "+name+" provider", output, err)
	}
	installed := m.toolStatus(ctx, candidate)
	if !installed.Installed {
		return installed, fmt.Errorf("%s installation completed but %s is not available in the Porto daemon PATH", name, candidate.command)
	}
	return installed, nil
}

func (m *Manager) toolStatus(ctx context.Context, candidate tool) Status {
	status := Status{Name: candidate.name, Command: candidate.command}
	commandPath, err := m.toolPath(candidate)
	if err != nil {
		status.Message = m.missingToolMessage(candidate)
		return status
	}
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: commandPath, Args: candidate.args})
	if err != nil {
		status.Message = runtimes.CommandError("inspect "+candidate.name+" provider", output, err).Error()
		return status
	}
	status.Installed = true
	status.Version = strings.TrimSpace(string(output))
	if candidate.name == "apple" {
		if err := ValidateAppleContainerVersion(status.Version); err != nil {
			status.Installed = false
			status.Message = err.Error()
		}
	}
	return status
}

func (m *Manager) toolPath(candidate tool) (string, error) {
	if candidate.name == "qemu" {
		return runtimes.ResolveQEMUPath(candidate.command, m.lookPath, m.getenv)
	}
	return m.lookPath(candidate.command)
}

func (m *Manager) missingToolMessage(candidate tool) string {
	if candidate.name != "qemu" {
		return fmt.Sprintf("%s is not installed", candidate.command)
	}
	switch m.goos {
	case "darwin":
		return "Install QEMU on your Mac with 'brew install qemu', then restart Porto"
	case "windows":
		return fmt.Sprintf("Install QEMU and add %s to PATH, then restart Porto", candidate.command)
	default:
		return "Install QEMU with your system package manager, then restart Porto"
	}
}

func findTool(name string) (tool, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, candidate := range tools {
		if candidate.name == name {
			return candidate, true
		}
	}
	return tool{}, false
}
