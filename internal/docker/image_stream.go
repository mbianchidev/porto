package docker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) StartImageSave(ctx context.Context, images []string) (runtimes.Process, error) {
	if len(images) == 0 {
		return nil, errors.New("at least one image is required")
	}
	args := []string{"save"}
	for _, image := range images {
		if err := validateObjectID(image); err != nil {
			return nil, err
		}
		args = append(args, normalizeNerdctlReference(image))
	}
	if runtime.GOOS == "windows" {
		backend, err := m.backend(ctx)
		if err != nil {
			return nil, err
		}
		if backend.limaInstance != "" {
			runner, ok := m.runner.(runtimes.ProcessRunner)
			if !ok {
				return nil, fmt.Errorf("%w: image archive process transport", ErrUnsupported)
			}
			// A Windows SSH console can translate binary newlines. Encode only this
			// transport leg and decode before serving the unchanged tar to Docker.
			const script = `set -eu
archive="$(mktemp)"
trap 'rm -f "$archive"' EXIT
trap 'exit 1' HUP INT TERM
nerdctl save --output "$archive" "$@"
base64 "$archive"
`
			process, err := runner.Start(ctx, runtimes.Command{
				Name: backend.name,
				Args: append([]string{"shell", "--workdir=/", backend.limaInstance, "--",
					"sh", "-c", script, "porto-save-archive"}, args[1:]...),
			})
			if err != nil {
				return nil, err
			}
			return &encodedImageSaveProcess{Process: process,
				output: &decodedArchiveReader{Reader: base64.NewDecoder(base64.StdEncoding, process.Stdout()), Closer: process.Stdout()}}, nil
		}
	}
	return m.startProcess(ctx, "save Porto images", args...)
}

type encodedImageSaveProcess struct {
	runtimes.Process
	output io.ReadCloser
}

func (process *encodedImageSaveProcess) Stdout() io.ReadCloser { return process.output }

type decodedArchiveReader struct {
	io.Reader
	io.Closer
}
