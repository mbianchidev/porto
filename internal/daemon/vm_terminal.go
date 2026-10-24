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
	"github.com/mbianchidev/porto/internal/process"
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
	bridgePTYTerminal(w, r, func(ctx context.Context) (*exec.Cmd, error) {
		return manager.ContainerTerminalCommand(ctx, id, shell, debug)
	}, "container terminal failed")
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

	sessionContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	command, err := commandFactory(sessionContext)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, err.Error())
		return
	}
	terminal, err := startHostPTY(sessionContext, command)
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
			cancel()
			_ = terminal.Close()
		}
		sessionErr = errors.Join(outputErr, <-processDone)
	case inputErr := <-inputDone:
		if err := process.Kill(command); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Debug("Terminal disconnect cleanup", "error", err)
		}
		cancel()
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
