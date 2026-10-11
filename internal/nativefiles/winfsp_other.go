//go:build !windows

package nativefiles

import (
	"context"
	"io"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func ProbeNativeDriver() Capability {
	return Capability{Driver: "WinFsp", Message: "WinFsp host mounts are available on Windows; use the host's existing native driver on this platform."}
}

func RunNativeMount(context.Context, io.Reader, io.Writer) error {
	return datafiles.ErrUnsupported
}
