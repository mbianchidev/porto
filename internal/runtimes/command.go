package runtimes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

type Command struct {
	Dir         string
	Name        string
	Args        []string
	Env         []string
	Stdin       []byte
	StdinReader io.Reader
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type OutputChunk struct {
	Stream string
	Data   []byte
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, command Command) (output []byte, err error) {
	finish := traceCommand(command)
	defer func() { finish(err) }()
	cmd := newExecCommand(ctx, command)
	closeInput, err := configureCommandInput(cmd, command)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	return cmd.CombinedOutput()
}

func (ExecRunner) RunStreaming(
	ctx context.Context,
	command Command,
	emit func(OutputChunk) error,
) (result []byte, err error) {
	finish := traceCommand(command)
	defer func() { finish(err) }()
	cmd := newExecCommand(ctx, command)
	closeInput, err := configureCommandInput(cmd, command)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	output := &streamOutput{emit: emit}
	cmd.Stdout = chunkWriter{stream: "stdout", output: output}
	cmd.Stderr = chunkWriter{stream: "stderr", output: output}
	err = cmd.Run()
	return output.diagnostic, err
}

func traceCommand(command Command) func(error) {
	binary := filepath.Base(command.Name)
	started := time.Now()
	slog.Debug("Runtime command started", "binary", binary)
	return func(err error) {
		slog.Debug("Runtime command finished", "binary", binary, "duration", time.Since(started), "error", err)
	}
}

func newExecCommand(ctx context.Context, command Command) *exec.Cmd {
	cmd := process.NewCommand(ctx, command.Dir, command.Name, command.Args...)
	cmd.Env = process.WithEnvironment(os.Environ(), command.Env...)
	return cmd
}

func configureCommandInput(cmd *exec.Cmd, command Command) (func(), error) {
	if command.StdinReader != nil {
		if command.Stdin != nil {
			return nil, errors.New("command cannot use both byte and stream input")
		}
		cmd.Stdin = command.StdinReader
		return func() {}, nil
	}
	if command.Stdin != nil {
		cmd.Stdin = bytes.NewReader(command.Stdin)
	}
	return func() {}, nil
}

func CommandError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}

type streamOutput struct {
	mu         sync.Mutex
	emit       func(OutputChunk) error
	diagnostic []byte
}

type chunkWriter struct {
	stream string
	output *streamOutput
}

func (w chunkWriter) Write(data []byte) (int, error) {
	copyData := append([]byte(nil), data...)
	w.output.mu.Lock()
	defer w.output.mu.Unlock()
	const diagnosticLimit = 64 * 1024
	if remaining := diagnosticLimit - len(w.output.diagnostic); remaining > 0 {
		w.output.diagnostic = append(w.output.diagnostic, copyData[:min(len(copyData), remaining)]...)
	}
	if err := w.output.emit(OutputChunk{Stream: w.stream, Data: copyData}); err != nil {
		return 0, err
	}
	return len(data), nil
}
