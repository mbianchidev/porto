//go:build !windows

package docker

import "github.com/mbianchidev/porto/internal/windowscontainers"

func allocateWindowsPipes(windowscontainers.Installation, string, bool, bool, *windowsIOHub) error {
	return windowscontainers.ErrUnsupported
}
