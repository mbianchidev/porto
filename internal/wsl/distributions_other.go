//go:build !windows

package wsl

import (
	"context"
	"errors"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func platformDistributions(context.Context, runtimes.Runner) ([]Distribution, error) {
	return nil, errors.New("selected-distribution WSL integration requires Windows")
}
