//go:build !darwin

package nativefiles

import (
	"context"

	"github.com/porto-containers/porto/internal/datafiles"
)

func (m *Manager) webDAVCapability() Capability {
	return Capability{Message: "The native WebDAV filesystem is available only on macOS."}
}

func (m *Manager) mountWebDAVLocked(context.Context, *liveAttachment, bool) error {
	return datafiles.ErrUnsupported
}
