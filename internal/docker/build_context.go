package docker

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) streamUploadedBuildContext(ctx context.Context, request BuildRequest, args []string, emit func(runtimes.OutputChunk) error) (err error) {
	directory, err := os.MkdirTemp("", "porto-build-upload-")
	if err != nil {
		return fmt.Errorf("create build upload directory: %w", err)
	}
	defer func() {
		err = errors.Join(err, os.RemoveAll(directory))
	}()
	archive, err := os.CreateTemp(directory, "context-*.tar")
	if err != nil {
		return fmt.Errorf("create uploaded build archive: %w", err)
	}
	defer func() { err = errors.Join(err, archive.Close()) }()
	decompressed, err := compression.DecompressStream(request.ContextReader)
	if err != nil {
		return fmt.Errorf("invalid compressed build context: %w", err)
	}
	count, copyErr := io.Copy(archive, io.LimitReader(decompressed, maxBuildContextBytes+1))
	closeErr := decompressed.Close()
	if copyErr != nil || closeErr != nil {
		return fmt.Errorf("read uploaded build context: %w", errors.Join(copyErr, closeErr))
	}
	if count > maxBuildContextBytes {
		return errors.New("build context exceeds the 2 GiB expanded limit")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	contextDirectory := filepath.Join(directory, "context")
	if err := os.Mkdir(contextDirectory, 0o700); err != nil {
		return err
	}
	if err := extractDockerBuildContext(ctx, contextDirectory, archive); err != nil {
		return fmt.Errorf("invalid Docker build context: %w", err)
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return err
	}
	if backend.limaInstance != "" {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return err
		}
		const script = `set -eu
context="$(mktemp -d)"
trap 'rm -rf "$context"' EXIT
trap 'exit 1' HUP INT TERM
tar --no-same-owner -xf - -C "$context"
cd "$context"
exec_command="$1"; shift
"$exec_command" "$@" .
`
		return m.runStreamingCommand(ctx, buildTimeout, "build Porto image", runtimes.Command{
			Name: backend.name,
			Args: append([]string{"shell", "--workdir=/", backend.limaInstance, "--",
				"sh", "-c", script, "porto-build-upload", "nerdctl"}, args...),
			StdinReader: archive,
		}, emit)
	}
	return m.runStreamingCommand(ctx, buildTimeout, "build Porto image", runtimes.Command{
		Name: backend.name,
		Args: append(append(append([]string(nil), backend.prefix...), args...), "."),
		Dir:  contextDirectory,
	}, emit)
}

func extractDockerBuildContext(ctx context.Context, directory string, archive io.Reader) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	reader := tar.NewReader(archive)
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		header, err := reader.Next()
		if err == io.EOF {
			if entries == 0 {
				return errors.New("empty build archive")
			}
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(header.Name)
		if name == "." && header.Typeflag == tar.TypeDir {
			continue
		}
		if !confinedBuildArchivePath(name) {
			return fmt.Errorf("archive path escapes the build context: %q", header.Name)
		}
		entries++
		if entries > 100_000 {
			return errors.New("build archive exceeds 100000 entries")
		}
		if err := root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0o700); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(filepath.FromSlash(name), 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			file, err := root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, reader)
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if path.IsAbs(header.Linkname) || !confinedBuildArchivePath(path.Clean(path.Join(path.Dir(name), header.Linkname))) {
				return fmt.Errorf("symlink escapes the build context: %q", name)
			}
			if err := root.Symlink(filepath.FromSlash(header.Linkname), filepath.FromSlash(name)); err != nil {
				return err
			}
		case tar.TypeLink:
			if !confinedBuildArchivePath(path.Clean(header.Linkname)) {
				return fmt.Errorf("hard link escapes the build context: %q", name)
			}
			if err := root.Link(filepath.FromSlash(header.Linkname), filepath.FromSlash(name)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: build archive entry type %d", ErrUnsupported, header.Typeflag)
		}
	}
}

func confinedBuildArchivePath(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.HasPrefix(name, "../") && !path.IsAbs(name) && !strings.ContainsAny(name, "\\:\x00\r\n")
}
