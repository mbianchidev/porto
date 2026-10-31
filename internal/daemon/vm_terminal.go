package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type terminalResize struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func podTerminalSupported() bool {
	return hostPTYSupported()
}

func bridgeVMTerminal(w http.ResponseWriter, r *http.Request, name string) {
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return vmTerminalCommand(ctx, name), nil
	}, "Lima shell failed")
}

func bridgeContainerTerminal(
	w http.ResponseWriter,
	r *http.Request,
	manager *portodocker.Manager,
	id string,
	shell string,
	debug bool,
) {
	if manager.IsNativeWindows() {
		bridgeProcessTerminal(w, r, func(ctx context.Context) (runtimes.Process, error) {
			return manager.StartWindowsContainerTerminal(ctx, id, shell, debug)
		}, "container terminal failed")
		return
	}
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return manager.ContainerTerminalCommand(ctx, id, shell, debug)
	}, "container terminal failed")
}

type processTerminalResize struct {
	ctx     context.Context
	process runtimes.Process
}

func (p processTerminalResize) Resize(cols, rows int) error {
	resizable, ok := p.process.(interface {
		Resize(context.Context, uint32, uint32) error
	})
	if !ok {
		return errors.New("native terminal process does not support resize")
	}
	return resizable.Resize(p.ctx, uint32(cols), uint32(rows))
}

func bridgeProcessTerminal(
	w http.ResponseWriter,
	r *http.Request,
	processFactory func(context.Context) (runtimes.Process, error),
	startFailure string,
) {
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(terminalReadLimit)
	sessionContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Process cancellation must leave the transport alive for its final close frame.
	processContext, cancelProcess := context.WithCancel(sessionContext)
	defer cancelProcess()
	process, err := processFactory(processContext)
	if err != nil {
		slog.Warn(startFailure, "error", err)
		_ = connection.Close(websocket.StatusInternalError, startFailure)
		return
	}
	input, stdout, stderr := process.Stdin(), process.Stdout(), process.Stderr()
	defer input.Close()
	defer stdout.Close()
	defer stderr.Close()

	var writeMu sync.Mutex
	writeOutput := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeContext, cancelWrite := context.WithTimeout(sessionContext, 10*time.Second)
		defer cancelWrite()
		return connection.Write(writeContext, websocket.MessageBinary, data)
	}
	outputDone := make(chan error, 2)
	go streamPTYOutput(stdout, writeOutput, outputDone)
	go streamPTYOutput(stderr, writeOutput, outputDone)
	inputDone := make(chan error, 1)
	go func() {
		for {
			kind, data, readErr := connection.Read(sessionContext)
			if readErr != nil {
				inputDone <- readErr
				return
			}
			if kind == websocket.MessageText {
				handled, resizeErr := applyTerminalResize(processTerminalResize{processContext, process}, data)
				if resizeErr != nil {
					inputDone <- resizeErr
					return
				}
				if handled {
					continue
				}
			}
			if _, writeErr := input.Write(data); writeErr != nil {
				inputDone <- writeErr
				return
			}
		}
	}()

	var sessionErr error
	var interruptOnce sync.Once
	interrupt := func(err error) {
		interruptOnce.Do(func() {
			sessionErr = err
			cancelProcess()
			sessionErr = errors.Join(sessionErr, process.Kill())
		})
	}
	contextDone := sessionContext.Done()
	for remaining := 2; remaining > 0; {
		select {
		case outputErr := <-outputDone:
			remaining--
			if outputErr != nil {
				interrupt(outputErr)
			}
		case inputErr := <-inputDone:
			interrupt(inputErr)
			inputDone = nil
		case <-contextDone:
			interrupt(sessionContext.Err())
			contextDone = nil
		}
	}
	// Native Wait releases its subscription. Drain both output streams first
	// so the final console bytes cannot be discarded by a fast process exit.
	waitErr := process.Wait()
	if sessionErr == nil {
		sessionErr = waitErr
	}
	var exit interface{ ExitCode() int }
	if errors.As(sessionErr, &exit) {
		_ = connection.Close(websocket.StatusInternalError, fmt.Sprintf("terminal exited with exit code %d", exit.ExitCode()))
		return
	}
	if errors.Is(sessionErr, context.Canceled) || errors.Is(sessionErr, os.ErrClosed) {
		sessionErr = nil
	}
	if sessionErr != nil {
		slog.Warn("Terminal session ended", "error", sessionErr)
		_ = connection.Close(websocket.StatusInternalError, "terminal session ended")
		return
	}
	_ = connection.Close(websocket.StatusNormalClosure, "terminal session complete")
}

func bridgePodTerminal(w http.ResponseWriter, r *http.Request, args []string) {
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return podTerminalCommand(ctx, args), nil
	}, "kubectl exec failed")
}

func bridgeK9sTerminal(w http.ResponseWriter, r *http.Request, cluster kubernetes.Cluster) {
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return k9sTerminalCommand(ctx, cluster), nil
	}, "k9s failed")
}

func bridgeDiveTerminal(w http.ResponseWriter, r *http.Request, divePath, image, socketPath string) {
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return diveTerminalCommand(ctx, divePath, image, socketPath), nil
	}, "Dive image inspector failed")
}

func bridgePTYTerminal(
	w http.ResponseWriter,
	r *http.Request,
	commandFactory func(context.Context) (*exec.Cmd, error),
	startFailure string,
) {
	if !hostPTYSupported() {
		http.Error(w, "embedded terminals require Windows 10 1809 or newer with ConPTY, or a Unix PTY", http.StatusNotImplemented)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(terminalReadLimit)

	sessionContext, cancel := context.WithCancel(r.Context())
	defer cancel()
	processContext, cancelProcess := context.WithCancel(sessionContext)
	defer cancelProcess()
	command, err := commandFactory(processContext)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, err.Error())
		return
	}
	terminal, err := startHostPTY(processContext, command)
	if err != nil {
		slog.Warn(startFailure, "error", err)
		_ = connection.Close(websocket.StatusInternalError, startFailure)
		return
	}
	defer terminal.Close()

	var writeMu sync.Mutex
	writeOutput := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeContext, cancelWrite := context.WithTimeout(sessionContext, 10*time.Second)
		defer cancelWrite()
		return connection.Write(writeContext, websocket.MessageBinary, data)
	}

	outputDone := make(chan error, 1)
	go streamPTYOutput(terminal, writeOutput, outputDone)
	processDone := make(chan error, 1)
	go func() { processDone <- terminal.Wait() }()
	inputDone := make(chan error, 1)
	go func() {
		for {
			messageType, data, readErr := connection.Read(sessionContext)
			if readErr != nil {
				inputDone <- readErr
				return
			}
			if messageType == websocket.MessageText {
				handled, resizeErr := applyTerminalResize(terminal, data)
				if resizeErr != nil {
					inputDone <- resizeErr
					return
				}
				if handled {
					continue
				}
			}
			if _, writeErr := terminal.Write(data); writeErr != nil {
				inputDone <- writeErr
				return
			}
		}
	}()

	var sessionErr error
	select {
	case processErr := <-processDone:
		_ = terminal.Close()
		sessionErr = errors.Join(processErr, <-outputDone)
	case outputErr := <-outputDone:
		if outputErr != nil {
			cancelProcess()
			_ = terminal.Close()
		}
		sessionErr = errors.Join(outputErr, <-processDone)
	case inputErr := <-inputDone:
		cancelProcess()
		_ = terminal.Close()
		sessionErr = errors.Join(inputErr, <-outputDone, <-processDone)
	}
	var exit *exec.ExitError
	if errors.As(sessionErr, &exit) {
		_ = connection.Close(websocket.StatusInternalError, fmt.Sprintf("terminal exited with exit code %d", exit.ExitCode()))
		return
	}
	if errors.Is(sessionErr, context.Canceled) || errors.Is(sessionErr, os.ErrClosed) {
		sessionErr = nil
	}
	if sessionErr != nil {
		slog.Warn("Terminal session ended", "error", sessionErr)
		_ = connection.Close(websocket.StatusInternalError, "terminal session ended")
		return
	}
	_ = connection.Close(websocket.StatusNormalClosure, "terminal session complete")
}

func applyTerminalResize(terminal interface{ Resize(int, int) error }, data []byte) (bool, error) {
	var resize terminalResize
	if json.Unmarshal(data, &resize) != nil || resize.Type != "resize" {
		return false, nil
	}
	if resize.Cols < 1 || resize.Cols > 1000 || resize.Rows < 1 || resize.Rows > 1000 {
		return true, errors.New("terminal dimensions must be between 1 and 1000")
	}
	if err := terminal.Resize(resize.Cols, resize.Rows); err != nil {
		return true, fmt.Errorf("resize terminal: %w", err)
	}
	return true, nil
}

func streamPTYOutput(terminal io.Reader, write func([]byte) error, done chan<- error) {
	buffer := make([]byte, 32*1024)
	for {
		read, err := terminal.Read(buffer)
		if read > 0 {
			if writeErr := write(append([]byte(nil), buffer[:read]...)); writeErr != nil {
				done <- writeErr
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EIO) {
				done <- nil
			} else {
				done <- fmt.Errorf("read VM terminal output: %w", err)
			}
			return
		}
	}
}
