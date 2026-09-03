package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"

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
	args = append(args, "--")
	args = append(args, podTerminalShellCommand(shell)...)
	args = s.kubernetes.CommandArgs(runtimeContext(r), args...)
	bridgePodTerminal(w, r, args)
}

func (s *Server) dockerContainerTerminal(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	shell := strings.TrimSpace(r.URL.Query().Get("shell"))
	if shell == "" {
		shell = "sh"
	}
	if !allowedShell(shell) {
		http.Error(w, "shell must be sh, bash, ash, or an equivalent /bin path", http.StatusBadRequest)
		return
	}
	document, err := s.docker.InspectContainer(r.Context(), id)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	ready, err := containerTerminalReady(document)
	if err != nil {
		http.Error(w, "container runtime returned invalid terminal state", http.StatusBadGateway)
		return
	}
	if !ready {
		http.Error(w, "container must be running and unpaused to open a terminal", http.StatusConflict)
		return
	}
	bridgeContainerTerminal(w, r, s.docker, id, shell, queryBool(r, "debug"))
}

func containerTerminalReady(document json.RawMessage) (bool, error) {
	var inspected struct {
		State struct {
			Running bool `json:"Running"`
			Paused  bool `json:"Paused"`
		} `json:"State"`
	}
	if err := json.Unmarshal(document, &inspected); err != nil {
		return false, err
	}
	return inspected.State.Running && !inspected.State.Paused, nil
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
	command := exec.CommandContext(
		ctx,
		"limactl", "shell", "--tty=true", name, "--",
		"sh", "-lc", `cd "$HOME" && exec env PS1="$1 $ " sh -i`, "porto-shell", name,
	)
	command.Env = process.WithEnvironment(
		os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)
	return command
}

func k9sTerminalCommand(ctx context.Context, cluster kubernetes.Cluster) *exec.Cmd {
	command := exec.CommandContext(
		ctx,
		"k9s",
		"--kubeconfig", cluster.KubeconfigPath,
		"--context", cluster.Context,
		"--all-namespaces",
	)
	command.Env = process.WithEnvironment(
		os.Environ(),
		"KUBECONFIG="+cluster.KubeconfigPath,
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)
	return command
}

func podTerminalCommand(ctx context.Context, args []string) *exec.Cmd {
	command := exec.CommandContext(ctx, "kubectl", args...)
	command.Env = process.WithEnvironment(
		os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)
	return command
}

func podTerminalShellCommand(shell string) []string {
	return []string{shell, "-c", `TERM=xterm-256color COLORTERM=truecolor exec "$0" -i`, shell}
}

func allowedShell(shell string) bool {
	switch shell {
	case "sh", "bash", "ash", "/bin/sh", "/bin/bash", "/bin/ash":
		return true
	default:
		return false
	}
}
