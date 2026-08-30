package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const terminalReadLimit = 1024 * 1024

func (s *Server) kubernetesPodTerminal(w http.ResponseWriter, r *http.Request) {
	namespace := strings.TrimSpace(r.PathValue("namespace"))
	pod := strings.TrimSpace(r.PathValue("pod"))
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	shell := strings.TrimSpace(r.URL.Query().Get("shell"))
	if shell == "" {
		shell = "sh"
	}
	if namespace == "" || pod == "" || strings.ContainsAny(namespace+pod+container, "\x00\r\n") {
		http.Error(w, "invalid Kubernetes terminal target", http.StatusBadRequest)
		return
	}
	if !allowedShell(shell) {
		http.Error(w, "shell must be sh, bash, ash, or an equivalent /bin path", http.StatusBadRequest)
		return
	}

	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(terminalReadLimit)

	sessionContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := []string{"exec", "--stdin", "--tty", "--namespace", namespace, pod}
	if container != "" {
		args = append(args, "--container", container)
	}
	args = append(args, "--", shell)
	args = s.kubernetes.CommandArgs(runtimeContext(r), args...)
	command := exec.CommandContext(sessionContext, "kubectl", args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		_ = connection.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = connection.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_ = connection.Close(websocket.StatusInternalError, "kubectl exec failed")
		return
	}

	var writeMu sync.Mutex
	writeOutput := func(data []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeContext, cancelWrite := context.WithTimeout(sessionContext, 10*time.Second)
		defer cancelWrite()
		return connection.Write(writeContext, websocket.MessageBinary, data)
	}
	outputErrors := make(chan error, 2)
	go streamTerminalOutput(stdout, writeOutput, outputErrors)
	go streamTerminalOutput(stderr, writeOutput, outputErrors)
	outputDone := make(chan error, 1)
	go func() {
		outputDone <- errors.Join(<-outputErrors, <-outputErrors)
	}()

	inputDone := make(chan error, 1)
	go func() {
		defer stdin.Close()
		for {
			_, data, err := connection.Read(sessionContext)
			if err != nil {
				inputDone <- err
				return
			}
			if _, err := stdin.Write(data); err != nil {
				inputDone <- err
				return
			}
		}
	}()

	var sessionErr error
	select {
	case sessionErr = <-outputDone:
		sessionErr = errors.Join(sessionErr, command.Wait())
	case sessionErr = <-inputDone:
		cancel()
		sessionErr = errors.Join(sessionErr, <-outputDone, command.Wait())
	}
	cancel()
	if errors.Is(sessionErr, context.Canceled) {
		sessionErr = nil
	}
	if sessionErr != nil {
		_ = connection.Close(websocket.StatusInternalError, "terminal session ended")
		return
	}
	_ = connection.Close(websocket.StatusNormalClosure, "terminal session complete")
}

func streamTerminalOutput(reader io.Reader, write func([]byte) error, done chan<- error) {
	buffer := make([]byte, 32*1024)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			if writeErr := write(append([]byte(nil), buffer[:read]...)); writeErr != nil {
				done <- writeErr
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
			} else {
				done <- fmt.Errorf("read terminal output: %w", err)
			}
			return
		}
	}
}

func allowedShell(shell string) bool {
	switch shell {
	case "sh", "bash", "ash", "/bin/sh", "/bin/bash", "/bin/ash":
		return true
	default:
		return false
	}
}
