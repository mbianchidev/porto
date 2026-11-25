//go:build !linux

package runtimefiles

import (
	"context"
	"fmt"
	"io"

	"github.com/porto-containers/porto/internal/datafiles"
)

func containerChanges(context.Context, Descriptor, io.Writer) error {
	return fmt.Errorf("%w: container filesystem changes require the Linux runtime helper", datafiles.ErrUnsupported)
}
