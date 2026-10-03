//go:build !windows

package nativefiles

import (
	"context"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func (m *Manager) windowsCapability(context.Context) Capability { return ProbeNativeDriver() }
func (m *Manager) attachWindowsLocked(context.Context, Target, bool) (Attachment, error) {
	return Attachment{}, datafiles.ErrUnsupported
}
func verifyWindowsMount(Attachment) error { return datafiles.ErrUnsupported }
func (m *Manager) detachWindowsLocked(context.Context, *liveAttachment) error {
	return datafiles.ErrUnsupported
}
