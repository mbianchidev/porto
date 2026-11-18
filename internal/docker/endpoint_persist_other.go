//go:build !darwin

package docker

import "errors"

func installEndpointLaunchDaemon(_, _ string) error {
	return errors.New("persisting the canonical Docker endpoint across reboots is available only on macOS")
}

func removeEndpointLaunchDaemon() error {
	return nil
}

// EndpointPersisted reports whether the boot-time job that restores the canonical endpoint is installed.
func EndpointPersisted() bool {
	return false
}
