//go:build windows

package daemon

import (
	"net/http"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
)

func podTerminalSupported() bool {
	return false
}

func bridgeVMTerminal(w http.ResponseWriter, _ *http.Request, _ string) {
	http.Error(w, "interactive VM terminals require a PTY-capable host", http.StatusNotImplemented)
}

func bridgeContainerTerminal(w http.ResponseWriter, _ *http.Request, _ *portodocker.Manager, _ string, _ string, _ bool) {
	http.Error(w, "interactive container terminals require a PTY-capable host", http.StatusNotImplemented)
}

func bridgePodTerminal(w http.ResponseWriter, _ *http.Request, _ []string) {
	http.Error(w, "interactive pod terminals require a PTY-capable host", http.StatusNotImplemented)
}

func bridgeK9sTerminal(w http.ResponseWriter, _ *http.Request, _ kubernetes.Cluster) {
	http.Error(w, "embedded k9s terminals require a PTY-capable host; use 'porto kubernetes terminal <cluster>'", http.StatusNotImplemented)
}
