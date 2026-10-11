//go:build !linux

package runtimefiles

import (
	"context"
	"io"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func dispatchRuntimeNamespace(context.Context, Descriptor, datafiles.Request, io.Reader, io.Writer) (bool, error) {
	return false, nil
}
