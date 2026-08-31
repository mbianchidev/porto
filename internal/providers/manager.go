package providers

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type Manager struct {
	runner runtimes.Runner
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
	{name: "kind", command: "kind", args: []string{"version"}, formula: "kind"},
	{name: "k9s", command: "k9s", args: []string{"version", "--short"}, formula: "k9s"},
	{name: "k0s", command: "limactl", args: []string{"--version"}, formula: "lima"},
}

func New(runner runtimes.Runner) *Manager {
	if runner == nil {
		runner = runtimes.ExecRunner{}
	}
	return &Manager{runner: runner}
}

func (m *Manager) Status(ctx context.Context) []Status {
	statuses := make([]Status, 0, len(tools))
	for _, candidate := range tools {
		statuses = append(statuses, m.toolStatus(ctx, candidate))
	}
	return statuses
}

func (m *Manager) Install(ctx context.Context, name string) (Status, error) {
	candidate, ok := findTool(name)
	if !ok {
		return Status{}, fmt.Errorf("unknown runtime provider %q", name)
	}
	current := m.toolStatus(ctx, candidate)
	if current.Installed {
		return current, nil
	}
	if runtime.GOOS != "darwin" {
		return current, fmt.Errorf("automatic %s installation is currently supported on macOS; install %s and retry", name, candidate.command)
	}
	if _, err := exec.LookPath("brew"); err != nil {
		return current, errors.New("Homebrew is required for automatic provider installation on macOS")
	}
	installContext, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	output, err := m.runner.Run(installContext, runtimes.Command{
		Name: "brew",
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
	if _, err := exec.LookPath(candidate.command); err != nil {
		status.Message = fmt.Sprintf("%s is not installed", candidate.command)
		return status
	}
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: candidate.command, Args: candidate.args})
	if err != nil {
		status.Message = runtimes.CommandError("inspect "+candidate.name+" provider", output, err).Error()
		return status
	}
	status.Installed = true
	status.Version = strings.TrimSpace(string(output))
	return status
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
