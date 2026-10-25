//go:build !windows

package windowscontainers

import "net/http"

func Gate(next http.Handler) http.Handler { return next }
