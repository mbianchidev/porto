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

	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/process"
)

type Command struct {
	Dir         string
	Name        string
	Args        []string
	Env         []string
	Stdin       []byte
	StdinReader io.Reader
	// Structured reads must not mix successful stdout with runtime diagnostics.
	StdoutOnly bool
	// Local ownership and cleanup must not depend on proxy-vault availability.
	Network       bool
	NetworkPolicy *networking.Manager
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
	command, err = withNetworkEnvironment(command)
	if err != nil {
		return nil, err
	}
	cmd := newExecCommand(ctx, command)
	closeInput, err := configureCommandInput(cmd, command)
	if err != nil {
		return nil, err
	}
	defer closeInput()
	if !command.StdoutOnly {
		return cmd.CombinedOutput()
	}
	diagnostics := &streamOutput{emit: func(OutputChunk) error { return nil }}
	cmd.Stderr = chunkWriter{stream: "stderr", output: diagnostics}
	stdout, err := cmd.Output()
	if err != nil {
		return diagnostics.diagnostic, err
	}
	if len(diagnostics.diagnostic) > 0 {
		slog.Warn("Runtime command emitted stderr diagnostics", "binary", filepath.Base(command.Name), "capturedBytes", len(diagnostics.diagnostic))
	}
	return stdout, nil
}

func (ExecRunner) RunStreaming(
	ctx context.Context,
	command Command,
	emit func(OutputChunk) error,
) (result []byte, err error) {
	finish := traceCommand(command)
	defer func() { finish(err) }()
	command, err = withNetworkEnvironment(command)
	if err != nil {
		return nil, err
	}
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

func withNetworkEnvironment(command Command) (Command, error) {
	binary := strings.TrimSuffix(strings.ToLower(filepath.Base(command.Name)), ".exe")
	required := command.Network
	switch binary {
	case "limactl":
		required = required || (len(command.Args) > 0 && (command.Args[0] == "start" || command.Args[0] == "create"))
	case "docker", "nerdctl":
		required = required || RegistryOperation(command.Args)
	}
	if !required {
		return command, nil
	}
	manager := command.NetworkPolicy
	if manager == nil {
		var err error
		manager, err = networking.Default()
		if err != nil {
			return command, err
		}
	}
	environment, err := manager.Environment()
	if err != nil {
		return command, fmt.Errorf("configure runtime networking: %w", err)
	}
	command.Env = append(environment, command.Env...)
	return command, nil
}

func RegistryOperation(arguments []string) bool {
	for len(arguments) > 0 && strings.HasPrefix(arguments[0], "-") {
		flag, _, inline := strings.Cut(arguments[0], "=")
		switch flag {
		case "--debug", "-D", "--debug-full", "--experimental", "--insecure-registry",
			"--kube-hide-dupe", "--selinux-enabled", "--tls", "--tlsverify":
			arguments = arguments[1:]
		case "--address", "-a", "-H", "--host", "--namespace", "-n", "--snapshotter", "--storage-driver",
			"--cni-path", "--cni-netconfpath", "--data-root", "--cgroup-manager", "--hosts-dir", "--log-file",
			"--host-gateway-ip", "--bridge-ip", "--cdi-spec-dirs", "--userns-remap",
			"--global-dns", "--global-dns-opts", "--global-dns-search",
			"--context", "-c", "--config", "--tlscacert", "--tlscert", "--tlskey", "--log-level", "-l":
			if !inline {
				if len(arguments) < 2 {
					return false
				}
				arguments = arguments[1:]
			}
			arguments = arguments[1:]
		default:
			return false
		}
	}
	if len(arguments) == 0 {
		return false
	}
	switch arguments[0] {
	case "pull", "push", "build", "buildx", "compose", "login", "search", "run", "create":
		return true
	case "container", "image":
		return RegistryOperation(arguments[1:])
	default:
		return false
	}
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
	message := networking.Redact(strings.TrimSpace(string(output)))
	action = networking.Redact(action)
	if err != nil {
		err = redactedCommandError{err: err}
	}
	if message == "" {
		return fmt.Errorf("%s: %w", action, err)
	}
	return fmt.Errorf("%s: %w: %s", action, err, message)
}

type redactedCommandError struct{ err error }

func (e redactedCommandError) Error() string { return networking.Redact(e.err.Error()) }
func (e redactedCommandError) Unwrap() error { return e.err }

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
