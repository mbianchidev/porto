package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/mbianchidev/porto/internal/runtimes"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const limaIOBridgeScript = `set -eu
base="${XDG_RUNTIME_DIR:-/tmp}/porto-container-fifos"
umask 077
mkdir -p "$base"
dir="$(mktemp -d "$base/porto-io.XXXXXX")"
cleanup() {
  rm -rf "$dir"
}
trap cleanup EXIT HUP INT TERM
stdin_path=""
stderr_path=""
log_path="$3"
if [ -n "$log_path" ]; then
  mkdir -p "$(dirname "$log_path")"
  touch "$log_path"
fi
if [ "$2" = "1" ]; then
  stdin_path="$dir/stdin"
  mkfifo "$stdin_path"
fi
stdout_path="$dir/stdout"
mkfifo "$stdout_path"
if [ "$1" != "1" ]; then
  stderr_path="$dir/stderr"
  mkfifo "$stderr_path"
fi
printf 'PORTO_IO\t%s\t%s\t%s\n' "$stdin_path" "$stdout_path" "$stderr_path"
if [ -n "$stdin_path" ]; then
  cat > "$stdin_path" &
fi
if [ -n "$log_path" ]; then
  tee -a "$log_path" < "$stdout_path" &
else
  cat "$stdout_path" &
fi
if [ -n "$stderr_path" ]; then
  if [ -n "$log_path" ]; then
    tee -a "$log_path" < "$stderr_path" >&2 &
  else
    cat "$stderr_path" >&2 &
  fi
fi
wait
`

type directProcessIO struct {
	creator cio.Creator
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	bridge  runtimes.Process
	cleanup func() error
}

type directContainerProcess struct {
	process containerd.Process
	wait    <-chan containerd.ExitStatus
	io      directProcessIO

	waitOnce sync.Once
	waitErr  error
}

type containerExitError struct {
	code int
}

func (e *containerExitError) Error() string {
	return fmt.Sprintf("container process exited with code %d", e.code)
}

func (e *containerExitError) ExitCode() int {
	return e.code
}

func (p *directContainerProcess) Stdin() io.WriteCloser {
	return p.io.stdin
}

func (p *directContainerProcess) Stdout() io.ReadCloser {
	return p.io.stdout
}

func (p *directContainerProcess) Stderr() io.ReadCloser {
	return p.io.stderr
}

func (p *directContainerProcess) Wait() error {
	p.waitOnce.Do(func() {
		status, ok := <-p.wait
		if !ok {
			p.waitErr = errors.New("containerd process wait channel closed without an exit status")
		} else {
			code, _, err := status.Result()
			if err != nil {
				p.waitErr = err
			} else if code != 0 {
				p.waitErr = &containerExitError{code: int(code)}
			}
		}
		p.waitErr = errors.Join(p.waitErr, p.cleanup())
	})
	return p.waitErr
}

func (p *directContainerProcess) Kill() error {
	return p.process.Kill(context.Background(), syscall.SIGKILL)
}

func (p *directContainerProcess) PID() int {
	return int(p.process.Pid())
}

func (p *directContainerProcess) Resize(ctx context.Context, width, height uint32) error {
	return p.process.Resize(ctx, width, height)
}

func (p *directContainerProcess) cleanup() error {
	cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ioSet := p.process.IO(); ioSet != nil {
		ioSet.Cancel()
		ioSet.Wait()
	}
	var cleanupErr error
	if _, err := p.process.Delete(cleanupContext); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if p.io.cleanup != nil {
		cleanupErr = errors.Join(cleanupErr, p.io.cleanup())
	}
	if p.io.bridge != nil {
		_ = p.io.bridge.Kill()
		_ = p.io.bridge.Wait()
	}
	return cleanupErr
}

func (r *grpcContainerRuntime) StartExec(
	ctx context.Context,
	request ExecRequest,
) (runtimes.Process, error) {
	if r.client == nil {
		return nil, fmt.Errorf("%w: high-level containerd client is unavailable", ErrUnavailable)
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	container, err := r.client.LoadContainer(namespacedContext, request.ContainerID)
	if err != nil {
		return nil, containerdOperationError("load metadata for exec in", request.ContainerID, err)
	}
	task, err := container.Task(namespacedContext, nil)
	if err != nil {
		return nil, containerdOperationError("load task for exec in", request.ContainerID, err)
	}
	processSpec, err := r.execProcessSpec(namespacedContext, container, task, request)
	if err != nil {
		return nil, err
	}
	execID, err := randomResourceName()
	if err != nil {
		return nil, err
	}
	processIO, err := r.newDirectProcessIO(ctx, execID, request.TTY, request.AttachStdin, "")
	if err != nil {
		return nil, err
	}
	process, err := task.Exec(namespacedContext, execID, processSpec, processIO.creator)
	if err != nil {
		_ = processIO.cleanup()
		return nil, containerdOperationError("create exec in", request.ContainerID, err)
	}
	wait, err := process.Wait(namespacedContext)
	if err != nil {
		_, _ = process.Delete(context.Background())
		_ = processIO.cleanup()
		return nil, containerdOperationError("wait for exec in", request.ContainerID, err)
	}
	if err := process.Start(namespacedContext); err != nil {
		_, _ = process.Delete(context.Background())
		_ = processIO.cleanup()
		return nil, containerdOperationError("start exec in", request.ContainerID, err)
	}
	return &directContainerProcess{process: process, wait: wait, io: processIO}, nil
}

func (r *grpcContainerRuntime) StartAttached(
	ctx context.Context,
	id string,
	attachStdin bool,
) (runtimes.Process, error) {
	if r.client == nil {
		return nil, fmt.Errorf("%w: high-level containerd client is unavailable", ErrUnavailable)
	}
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	container, err := r.client.LoadContainer(namespacedContext, id)
	if err != nil {
		return nil, containerdOperationError("load metadata for attached start of", id, err)
	}
	labels, err := container.Labels(namespacedContext)
	if err != nil {
		return nil, containerdOperationError("read metadata for attached start of", id, err)
	}
	if labels[portoManagedLabel] != portoRuntimeVersion {
		return nil, fmt.Errorf("%w: container %q attached I/O is owned by its compatibility runtime", ErrUnsupported, id)
	}
	spec, err := container.Spec(namespacedContext)
	if err != nil {
		return nil, containerdOperationError("read spec for attached start of", id, err)
	}
	terminal := spec.Process != nil && spec.Process.Terminal
	processIO, err := r.newDirectProcessIO(ctx, id, terminal, attachStdin, labels[portoLogPathLabel])
	if err != nil {
		return nil, err
	}
	if existing, taskErr := container.Task(namespacedContext, nil); taskErr == nil {
		status, statusErr := existing.Status(namespacedContext)
		if statusErr != nil {
			_ = processIO.cleanup()
			return nil, containerdOperationError("inspect task for attached start of", id, statusErr)
		}
		if status.Status == containerd.Running ||
			status.Status == containerd.Paused ||
			status.Status == containerd.Pausing {
			_ = processIO.cleanup()
			return nil, fmt.Errorf("%w: container %q is already running", ErrConflict, id)
		}
		if _, deleteErr := existing.Delete(namespacedContext); deleteErr != nil {
			_ = processIO.cleanup()
			return nil, containerdOperationError("delete stopped task for attached start of", id, deleteErr)
		}
	}
	task, err := container.NewTask(namespacedContext, processIO.creator)
	if err != nil {
		_ = processIO.cleanup()
		return nil, containerdOperationError("create attached task for", id, err)
	}
	wait, err := task.Wait(namespacedContext)
	if err != nil {
		_, _ = task.Delete(context.Background())
		_ = processIO.cleanup()
		return nil, containerdOperationError("wait for attached task", id, err)
	}
	if err := task.Start(namespacedContext); err != nil {
		_, _ = task.Delete(context.Background())
		_ = processIO.cleanup()
		return nil, containerdOperationError("start attached task", id, err)
	}
	return &directContainerProcess{process: task, wait: wait, io: processIO}, nil
}

func (r *grpcContainerRuntime) execProcessSpec(
	ctx context.Context,
	container containerd.Container,
	task containerd.Task,
	request ExecRequest,
) (*specs.Process, error) {
	taskSpec, err := task.Spec(ctx)
	if err != nil {
		return nil, containerdOperationError("read task spec for exec in", request.ContainerID, err)
	}
	if taskSpec.Process == nil {
		return nil, fmt.Errorf("%w: container %q task has no process configuration", ErrConflict, request.ContainerID)
	}
	process := *taskSpec.Process
	process.Args = append([]string(nil), request.Command...)
	process.Env = mergeExecEnvironment(process.Env, request.Environment)
	process.Terminal = request.TTY
	process.ConsoleSize = nil
	if request.WorkingDir != "" {
		process.Cwd = request.WorkingDir
	}
	if request.User != "" {
		uid, gid, ok := numericContainerUser(request.User)
		if !ok {
			if r.lima != "" {
				return nil, fmt.Errorf(
					"%w: named exec users require rootfs resolution beside Lima containerd",
					ErrUnsupported,
				)
			}
			info, infoErr := container.Info(ctx)
			if infoErr != nil {
				return nil, containerdOperationError("read metadata for exec in", request.ContainerID, infoErr)
			}
			specCopy := *taskSpec
			specCopy.Process = &process
			if userErr := oci.WithUser(request.User)(
				ctx,
				r.client,
				&info,
				&specCopy,
			); userErr != nil {
				return nil, fmt.Errorf("resolve exec user %q: %w", request.User, userErr)
			}
			process = *specCopy.Process
		} else {
			process.User.UID = uid
			process.User.GID = gid
		}
	}
	return &process, nil
}

func mergeExecEnvironment(base, overrides []string) []string {
	result := append([]string(nil), base...)
	positions := make(map[string]int, len(result))
	for index, entry := range result {
		key, _, _ := strings.Cut(entry, "=")
		positions[key] = index
	}
	for _, entry := range overrides {
		key, _, _ := strings.Cut(entry, "=")
		if index, ok := positions[key]; ok {
			result[index] = entry
			continue
		}
		positions[key] = len(result)
		result = append(result, entry)
	}
	return result
}

func numericContainerUser(value string) (uint32, uint32, bool) {
	uidText, gidText, hasGID := strings.Cut(value, ":")
	uid, err := strconv.ParseUint(uidText, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	gid := uid
	if hasGID {
		parsed, err := strconv.ParseUint(gidText, 10, 32)
		if err != nil {
			return 0, 0, false
		}
		gid = parsed
	}
	return uint32(uid), uint32(gid), true
}

func (r *grpcContainerRuntime) newDirectProcessIO(
	ctx context.Context,
	id string,
	terminal,
	attachStdin bool,
	logPath string,
) (directProcessIO, error) {
	if r.lima != "" {
		return r.newLimaProcessIO(ctx, id, terminal, attachStdin, logPath)
	}
	if r.fifoDir == "" {
		return directProcessIO{}, fmt.Errorf("%w: containerd FIFO directory is unavailable", ErrUnsupported)
	}
	if err := os.MkdirAll(r.fifoDir, 0o700); err != nil {
		return directProcessIO{}, fmt.Errorf("create containerd FIFO directory: %w", err)
	}
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	stdoutTarget := io.Writer(stdoutWriter)
	stderrTarget := io.Writer(stderrWriter)
	var logFile *os.File
	if logPath != "" {
		if err := r.prepareDirectLogPath(ctx, logPath); err != nil {
			return directProcessIO{}, err
		}
		var err error
		logFile, err = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return directProcessIO{}, fmt.Errorf("open direct attached log %q: %w", logPath, err)
		}
		stdoutTarget = io.MultiWriter(stdoutWriter, logFile)
		stderrTarget = io.MultiWriter(stderrWriter, logFile)
	}
	options := []cio.Opt{
		cio.WithStreams(stdinReader, stdoutTarget, stderrTarget),
		cio.WithFIFODir(r.fifoDir),
	}
	if terminal {
		options = append(options, cio.WithTerminal)
	}
	return directProcessIO{
		creator: cio.NewCreator(options...),
		stdin:   stdinWriter,
		stdout:  stdoutReader,
		stderr:  stderrReader,
		cleanup: func() error {
			return errors.Join(
				stdinReader.Close(),
				stdinWriter.Close(),
				stdoutReader.Close(),
				stdoutWriter.Close(),
				stderrReader.Close(),
				stderrWriter.Close(),
				closeOptionalFile(logFile),
			)
		},
	}, nil
}

func (r *grpcContainerRuntime) newLimaProcessIO(
	ctx context.Context,
	id string,
	terminal,
	attachStdin bool,
	logPath string,
) (directProcessIO, error) {
	runner, ok := r.runner.(runtimes.ProcessRunner)
	if !ok {
		return directProcessIO{}, fmt.Errorf("%w: Lima FIFO bridge requires process streaming", ErrUnsupported)
	}
	bridge, err := runner.Start(ctx, runtimes.Command{
		Name: "limactl",
		Args: []string{
			"shell", r.lima, "--", "sh", "-c", limaIOBridgeScript,
			"porto-container-io",
			boolShellFlag(terminal),
			boolShellFlag(attachStdin),
			logPath,
		},
	})
	if err != nil {
		return directProcessIO{}, fmt.Errorf("start Lima container I/O bridge: %w", err)
	}
	stdout := bridge.Stdout()
	reader := bufio.NewReader(stdout)
	ready, err := reader.ReadString('\n')
	if err != nil {
		_ = bridge.Kill()
		return directProcessIO{}, errors.Join(
			fmt.Errorf("read Lima container I/O bridge handshake: %w", err),
			bridge.Wait(),
		)
	}
	fields := strings.Split(strings.TrimSuffix(ready, "\n"), "\t")
	if len(fields) != 4 || fields[0] != "PORTO_IO" {
		_ = bridge.Kill()
		return directProcessIO{}, errors.Join(
			fmt.Errorf("invalid Lima container I/O bridge handshake %q", strings.TrimSpace(ready)),
			bridge.Wait(),
		)
	}
	config := cio.Config{
		Stdin:    fields[1],
		Stdout:   fields[2],
		Stderr:   fields[3],
		Terminal: terminal,
	}
	return directProcessIO{
		creator: func(string) (cio.IO, error) {
			return &staticContainerIO{config: config}, nil
		},
		stdin:  bridge.Stdin(),
		stdout: &bufferedReadCloser{Reader: reader, closer: stdout},
		stderr: bridge.Stderr(),
		bridge: bridge,
		cleanup: func() error {
			return nil
		},
	}, nil
}

func closeOptionalFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func boolShellFlag(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

type staticContainerIO struct {
	config cio.Config
}

func (i *staticContainerIO) Config() cio.Config { return i.config }
func (i *staticContainerIO) Cancel()            {}
func (i *staticContainerIO) Wait()              {}
func (i *staticContainerIO) Close() error       { return nil }

type bufferedReadCloser struct {
	*bufio.Reader
	closer io.Closer
}

func (r *bufferedReadCloser) Close() error {
	return r.closer.Close()
}
