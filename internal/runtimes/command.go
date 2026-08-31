package runtimes

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type Command struct {
	Dir   string
	Name  string
	Args  []string
	Env   []string
	Stdin []byte
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command.Name, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append(os.Environ(), command.Env...)
	if command.Stdin != nil {
		cmd.Stdin = bytes.NewReader(command.Stdin)
	}
	return cmd.CombinedOutput()
}

func CommandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}
