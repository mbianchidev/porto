//go:build !windows

package windowscontainers

import (
	"context"
	"runtime"
)

func Probe(context.Context) (Host, error) {
	return Host{OS: runtime.GOOS, Architecture: runtime.GOARCH}, nil
}
