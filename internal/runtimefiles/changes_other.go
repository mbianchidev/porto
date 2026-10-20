//go:build !linux

package runtimefiles

import (
	"context"
	"fmt"
	"io"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func containerChanges(context.Context, Descriptor, io.Writer) error {
	return fmt.Errorf("%w: container filesystem changes require the Linux runtime helper", datafiles.ErrUnsupported)
}
