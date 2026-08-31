package docker

import (
	"context"
	"errors"
	"runtime"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) StartImageSave(ctx context.Context, images []string) (runtimes.Process, error) {
	if len(images) == 0 {
		return nil, errors.New("at least one image is required")
	}
	args := []string{"save", "--platform", "linux/" + runtime.GOARCH}
	for _, image := range images {
		if err := validateObjectID(image); err != nil {
			return nil, err
		}
		args = append(args, normalizeNerdctlReference(image))
	}
	return m.startProcess(ctx, "save Porto images", args...)
}
