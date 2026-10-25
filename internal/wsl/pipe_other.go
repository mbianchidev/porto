//go:build !windows

package wsl

import (
	"context"
	"errors"
	"io"
)

func PipeStdio(context.Context, string, io.Reader, io.Writer) error {
	return errors.New("WSL Docker named-pipe transport requires Windows")
}
