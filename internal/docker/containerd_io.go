package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
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
	creator      cio.Creator
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	stderr       io.ReadCloser
	bridge       runtimes.Process
	cleanup      func() error
	finishOutput func()
}

type directContainerProcess struct {
	process      containerd.Process
	wait         <-chan containerd.ExitStatus
	io           directProcessIO
	deleteOnWait bool
	namespace    string

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
	return p.process.Kill(withContainerdNamespace(context.Background(), p.namespace), syscall.SIGKILL)
}

func (p *directContainerProcess) PID() int {
	return int(p.process.Pid())
}

func (p *directContainerProcess) KillOnDisconnect() bool {
	return p.deleteOnWait
}

func (p *directContainerProcess) Resize(ctx context.Context, width, height uint32) error {
	return p.process.Resize(withContainerdNamespace(ctx, p.namespace), width, height)
}

func (p *directContainerProcess) cleanup() error {
	cleanupContext, cancel := context.WithTimeout(
		withContainerdNamespace(context.Background(), p.namespace),
		10*time.Second,
	)
	defer cancel()
	if ioSet := p.process.IO(); ioSet != nil {
		ioSet.Cancel()
		ioSet.Wait()
	}
	var cleanupErr error
	if p.deleteOnWait {
		if _, err := p.process.Delete(cleanupContext); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
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

func newDirectContainerProcess(
	process containerd.Process,
	wait <-chan containerd.ExitStatus,
	processIO directProcessIO,
	deleteOnWait bool,
	namespace string,
) *directContainerProcess {
	exit := make(chan containerd.ExitStatus, 1)
	go func() {
		status, ok := <-wait
		if ioSet := process.IO(); ioSet != nil {
			ioSet.Wait()
		}
		if processIO.finishOutput != nil {
			processIO.finishOutput()
		}
		if ok {
			exit <- status
		}
		close(exit)
	}()
	return &directContainerProcess{
		process:      process,
		wait:         exit,
		io:           processIO,
		deleteOnWait: deleteOnWait,
		namespace:    namespace,
	}
}

func (r *grpcContainerRuntime) StartExec(
	ctx context.Context,
	request ExecRequest,
) (runtimes.Process, error) {
	if r.client == nil {
		return nil, fmt.Errorf("%w: high-level containerd client is unavailable", ErrUnavailable)
	}
	resolvedID, err := r.resolveContainerID(ctx, request.ContainerID)
	if err != nil {
		return nil, err
	}
	request.ContainerID = resolvedID
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
	wait, err := process.Wait(withContainerdNamespace(context.Background(), r.namespace))
	if err != nil {
		_, _ = process.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, containerdOperationError("wait for exec in", request.ContainerID, err)
	}
	if err := process.Start(namespacedContext); err != nil {
		_, _ = process.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, containerdOperationError("start exec in", request.ContainerID, err)
	}
	return newDirectContainerProcess(process, wait, processIO, true, r.namespace), nil
}

func (r *grpcContainerRuntime) StartAttached(
	ctx context.Context,
	id string,
	attachStdin bool,
) (runtimes.Process, error) {
	if r.client == nil {
		return nil, fmt.Errorf("%w: high-level containerd client is unavailable", ErrUnavailable)
	}
	resolvedID, err := r.resolveContainerID(ctx, id)
	if err != nil {
		return nil, err
	}
	id = resolvedID
	unlock := r.networkLocks.lock(id)
	defer unlock()
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
	networkResponse, err := r.containers.Get(
		namespacedContext,
		&containersapi.GetContainerRequest{ID: id},
	)
	if err != nil {
		return nil, containerdOperationError("read network metadata for attached start of", id, err)
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
	taskFound := false
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
		if err := r.cleanupNetworkRecord(ctx, networkResponse.GetContainer(), ""); err != nil {
			_ = processIO.cleanup()
			return nil, err
		}
		taskFound = true
		if _, deleteErr := existing.Delete(namespacedContext); deleteErr != nil {
			_ = processIO.cleanup()
			return nil, containerdOperationError("delete stopped task for attached start of", id, deleteErr)
		}
	} else if !errdefs.IsNotFound(taskErr) {
		_ = processIO.cleanup()
		return nil, containerdOperationError("inspect task for attached start of", id, taskErr)
	}
	if !taskFound {
		if err := r.cleanupNetworkRecord(ctx, networkResponse.GetContainer(), ""); err != nil {
			_ = processIO.cleanup()
			return nil, err
		}
	}
	task, err := container.NewTask(namespacedContext, processIO.creator)
	if err != nil {
		_ = processIO.cleanup()
		return nil, containerdOperationError("create attached task for", id, err)
	}
	netns := ""
	if task.Pid() > 0 {
		netns = fmt.Sprintf("/proc/%d/ns/net", task.Pid())
	}
	if err := r.reconcileNetworkRecord(ctx, networkResponse.GetContainer(), netns, true); err != nil {
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, _ = task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, errors.Join(err, networkCleanupErr)
	}
	if err := r.setRestartDesired(ctx, id, true); err != nil {
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, deleteErr := task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, errors.Join(err, networkCleanupErr, deleteErr)
	}
	wait, err := task.Wait(withContainerdNamespace(context.Background(), r.namespace))
	if err != nil {
		restartRollbackErr := r.setRestartDesired(context.Background(), id, false)
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, _ = task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, errors.Join(
			containerdOperationError("wait for attached task", id, err),
			restartRollbackErr,
			networkCleanupErr,
		)
	}
	if err := task.Start(namespacedContext); err != nil {
		restartRollbackErr := r.setRestartDesired(context.Background(), id, false)
		networkCleanupErr := r.cleanupNetworkRecord(context.Background(), networkResponse.GetContainer(), netns)
		_, _ = task.Delete(withContainerdNamespace(context.Background(), r.namespace))
		_ = processIO.cleanup()
		return nil, errors.Join(
			containerdOperationError("start attached task", id, err),
			restartRollbackErr,
			networkCleanupErr,
		)
	}
	return newDirectContainerProcess(task, wait, processIO, false, r.namespace), nil
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
		if r.lima != "" {
			return nil, fmt.Errorf(
				"%w: exec user overrides require backend-local rootfs resolution",
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
	var finishOnce sync.Once
	finishOutput := func() {
		finishOnce.Do(func() {
			_ = stdoutWriter.Close()
			_ = stderrWriter.Close()
		})
	}
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
		creator:      cio.NewCreator(options...),
		stdin:        stdinWriter,
		stdout:       stdoutReader,
		stderr:       stderrReader,
		finishOutput: finishOutput,
		cleanup: func() error {
			finishOutput()
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
	var finishOnce sync.Once
	return directProcessIO{
		creator: func(string) (cio.IO, error) {
			return &staticContainerIO{config: config}, nil
		},
		stdin:  bridge.Stdin(),
		stdout: &bufferedReadCloser{Reader: reader, closer: stdout},
		stderr: bridge.Stderr(),
		bridge: bridge,
		finishOutput: func() {
			finishOnce.Do(func() {
				_ = bridge.Stdin().Close()
				time.AfterFunc(2*time.Second, func() {
					_ = bridge.Kill()
				})
			})
		},
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
