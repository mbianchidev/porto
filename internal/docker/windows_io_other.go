//go:build !windows

package docker

import "github.com/porto-containers/porto/internal/windowscontainers"

func allocateWindowsPipes(windowscontainers.Installation, string, bool, bool, *windowsIOHub) error {
	return windowscontainers.ErrUnsupported
}
