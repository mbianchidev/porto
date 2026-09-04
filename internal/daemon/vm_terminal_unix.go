//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/creack/pty"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
)

type terminalResize struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func podTerminalSupported() bool {
	return true
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

func bridgePTYTerminal(
	w http.ResponseWriter,
	r *http.Request,
	commandFactory func(context.Context) (*exec.Cmd, error),
	startFailure string,
) {
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(terminalReadLimit)

	sessionContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	command, err := commandFactory(sessionContext)
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, err.Error())
		return
	}
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: 120, Rows: 32})
	if err != nil {
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
	case outputErr := <-outputDone:
		if outputErr != nil {
			cancel()
			_ = terminal.Close()
		}
		sessionErr = errors.Join(outputErr, command.Wait())
	case inputErr := <-inputDone:
		cancel()
		_ = terminal.Close()
		sessionErr = errors.Join(inputErr, <-outputDone, command.Wait())
	}
	cancel()
	if errors.Is(sessionErr, context.Canceled) || errors.Is(sessionErr, os.ErrClosed) {
		sessionErr = nil
	}
	if sessionErr != nil {
		_ = connection.Close(websocket.StatusInternalError, "terminal session ended")
		return
	}
	_ = connection.Close(websocket.StatusNormalClosure, "terminal session complete")
}

func applyTerminalResize(terminal *os.File, data []byte) (bool, error) {
	var resize terminalResize
	if json.Unmarshal(data, &resize) != nil || resize.Type != "resize" {
		return false, nil
	}
	if resize.Cols < 1 || resize.Cols > 1000 || resize.Rows < 1 || resize.Rows > 1000 {
		return true, nil
	}
	if err := pty.Setsize(terminal, &pty.Winsize{Cols: uint16(resize.Cols), Rows: uint16(resize.Rows)}); err != nil {
		return true, fmt.Errorf("resize VM terminal: %w", err)
	}
	return true, nil
}

func streamPTYOutput(terminal *os.File, write func([]byte) error, done chan<- error) {
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
