package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/process"
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

	args := []string{"exec", "--stdin", "--tty", "--namespace", namespace, pod}
	if container != "" {
		args = append(args, "--container", container)
	}
	args = append(args, "--", shell)
	args = s.kubernetes.CommandArgs(runtimeContext(r), args...)
	bridgeTerminal(w, r, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "kubectl", args...)
	}, "kubectl exec failed")
}

func (s *Server) vmTerminal(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if !s.requireStandaloneVM(w, name) {
		return
	}
	bridgeVMTerminal(w, r, name)
}

func (s *Server) kubernetesClusterTerminal(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" || strings.ContainsAny(name, "\x00\r\n") {
		http.Error(w, "invalid Kubernetes cluster name", http.StatusBadRequest)
		return
	}
	clusters, err := s.clusters.List(r.Context())
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	var selected *kubernetes.Cluster
	for index := range clusters {
		if clusters[index].Name == name {
			selected = &clusters[index]
			break
		}
	}
	if selected == nil {
		http.Error(w, "Kubernetes cluster not found", http.StatusNotFound)
		return
	}
	if !strings.EqualFold(selected.State, "running") {
		http.Error(w, "Kubernetes cluster must be running to open k9s", http.StatusConflict)
		return
	}
	if _, err := exec.LookPath("k9s"); err != nil {
		http.Error(w, "k9s is not installed; install it with 'porto runtime install k9s'", http.StatusServiceUnavailable)
		return
	}
	bridgeK9sTerminal(w, r, *selected)
}

func vmTerminalCommand(ctx context.Context, name string) *exec.Cmd {
	return exec.CommandContext(
		ctx,
		"limactl", "shell", "--tty=true", name, "--",
		"sh", "-lc", `cd "$HOME" && exec env PS1="$1 $ " sh -i`, "porto-shell", name,
	)
}

func k9sTerminalCommand(ctx context.Context, cluster kubernetes.Cluster) *exec.Cmd {
	command := exec.CommandContext(
		ctx,
		"k9s",
		"--kubeconfig", cluster.KubeconfigPath,
		"--context", cluster.Context,
		"--all-namespaces",
	)
	command.Env = process.WithEnvironment(os.Environ(), "KUBECONFIG="+cluster.KubeconfigPath)
	return command
}

func bridgeTerminal(
	w http.ResponseWriter,
	r *http.Request,
	commandFactory func(context.Context) *exec.Cmd,
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
	command := commandFactory(sessionContext)
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
		_ = connection.Close(websocket.StatusInternalError, startFailure)
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
