//go:build !windows

package windowscontainers

import (
	"context"

	"github.com/mbianchidev/porto/internal/networking"
)

func ConfigureNetworking(context.Context, networking.RuntimeConfiguration) (bool, error) {
	return false, nil
}
func SaveNetworking(Installation, networking.RuntimeConfiguration) error { return ErrUnsupported }
func LoadNetworking(Installation) (*networking.RuntimeConfiguration, error) {
	return nil, ErrUnsupported
}
