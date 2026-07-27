package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/app"
)

const defaultDownTimeout = 2 * time.Minute

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
	runner  Runner
	timeout time.Duration
}

func New(runner Runner) *Integration {
	return newIntegration(runner, defaultDownTimeout)
}

func newIntegration(runner Runner, timeout time.Duration) *Integration {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Integration{runner: runner, timeout: timeout}
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

func (i *Integration) Down(ctx context.Context, project app.Project) error {
	file, err := configFile(project)
	if err != nil {
		return err
	}
	commandContext, cancel := context.WithTimeout(ctx, i.timeout)
	defer cancel()
	output, err := i.runner.Run(commandContext, Command{
		Dir:  project.Path,
		Name: "docker",
		Args: []string{"compose", "-f", file, "down", "--remove-orphans"},
		Env: []string{
			"PORT=" + strconv.Itoa(project.Port),
			"PORTO_PORT=" + strconv.Itoa(project.Port),
		},
	})
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("docker compose cleanup for %s timed out: %w", project.Name, commandContext.Err())
	}
	if err != nil {
		return commandError("docker compose cleanup for "+project.Name, output, err)
	}
	return nil
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
