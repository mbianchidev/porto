//go:build !linux

package runtimefiles

import (
	"context"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func withDirectory(ctx context.Context, descriptor Descriptor, _ bool, run func(Descriptor) error) error {
	if descriptor.Resource.Kind != "volume" {
		return datafiles.ErrUnsupported
	}
	return run(descriptor)
}

func attach(context.Context, Descriptor, bool) (Attachment, error) {
	return Attachment{}, datafiles.ErrUnsupported
}

func detach(context.Context, Descriptor, string) error {
	return datafiles.ErrUnsupported
}

func ensureVolumeIdle(context.Context, Descriptor) error {
	return datafiles.ErrUnsupported
}
