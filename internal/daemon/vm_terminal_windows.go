//go:build windows

package daemon

import (
	"net/http"

	"github.com/mbianchidev/porto/internal/kubernetes"
)

func podTerminalSupported() bool {
	return false
}

func bridgeVMTerminal(w http.ResponseWriter, _ *http.Request, _ string) {
	http.Error(w, "interactive VM terminals require a PTY-capable host", http.StatusNotImplemented)
}

func bridgePodTerminal(w http.ResponseWriter, _ *http.Request, _ []string) {
	http.Error(w, "interactive pod terminals require a PTY-capable host", http.StatusNotImplemented)
}

func bridgeK9sTerminal(w http.ResponseWriter, _ *http.Request, _ kubernetes.Cluster) {
	http.Error(w, "embedded k9s terminals require a PTY-capable host; use 'porto kubernetes terminal <cluster>'", http.StatusNotImplemented)
}
