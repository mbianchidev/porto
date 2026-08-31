//go:build windows

package daemon

import (
	"net/http"
)

func bridgeVMTerminal(w http.ResponseWriter, _ *http.Request, _ string) {
	http.Error(w, "interactive VM terminals require a PTY-capable host", http.StatusNotImplemented)
}
