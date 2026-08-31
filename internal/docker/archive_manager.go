package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const containerStatFormat = "%s\\x00%f\\x00%Y\\x00"

func (m *Manager) ContainerPathStat(ctx context.Context, id, containerPath string) (PathStat, error) {
	if err := validateContainerPath(containerPath); err != nil {
		return PathStat{}, err
	}
	output, err := m.ExecContainer(ctx, id, []string{
		"stat", "--printf", containerStatFormat, "--", containerPath,
	}, nil)
	if err != nil {
		return PathStat{}, err
	}
	fields := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(fields) != 3 {
		return PathStat{}, errors.New("container stat returned an invalid response")
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return PathStat{}, fmt.Errorf("decode container path size: %w", err)
	}
	unixMode, err := strconv.ParseUint(fields[1], 16, 32)
	if err != nil {
		return PathStat{}, fmt.Errorf("decode container path mode: %w", err)
	}
	modified, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return PathStat{}, fmt.Errorf("decode container path modification time: %w", err)
	}
	return PathStat{
		Name:    path.Base(path.Clean(containerPath)),
		Size:    size,
		Mode:    dockerFileMode(uint32(unixMode)),
		ModTime: time.Unix(modified, 0).UTC(),
	}, nil
}

func (m *Manager) StartArchiveUpload(
	ctx context.Context,
	id string,
	destination string,
	copyUIDGID bool,
) (runtimes.Process, error) {
	if err := validateContainerPath(destination); err != nil {
		return nil, err
	}
	command := []string{"tar", "-xpf", "-"}
	if !copyUIDGID {
		command = append(command, "--no-same-owner")
	}
	command = append(command, "-C", destination)
	return m.StartExec(ctx, ExecRequest{
		ContainerID:  id,
		Command:      command,
		Privileged:   true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
}

func (m *Manager) StartArchiveDownload(ctx context.Context, id, source string) (runtimes.Process, error) {
	if err := validateContainerPath(source); err != nil {
		return nil, err
	}
	clean := path.Clean(source)
	parent, name := path.Dir(clean), path.Base(clean)
	if clean == "/" {
		parent, name = "/", "."
	}
	return m.StartExec(ctx, ExecRequest{
		ContainerID:  id,
		Command:      []string{"tar", "-cpf", "-", "-C", parent, name},
		Privileged:   true,
		AttachStdout: true,
		AttachStderr: true,
	})
}

func validateContainerPath(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("container path is required")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("invalid container path")
	}
	return nil
}

func dockerFileMode(unixMode uint32) os.FileMode {
	mode := os.FileMode(unixMode & 0o777)
	switch unixMode & 0o170000 {
	case 0o040000:
		mode |= os.ModeDir
	case 0o120000:
		mode |= os.ModeSymlink
	case 0o010000:
		mode |= os.ModeNamedPipe
	case 0o140000:
		mode |= os.ModeSocket
	case 0o060000:
		mode |= os.ModeDevice
	case 0o020000:
		mode |= os.ModeDevice | os.ModeCharDevice
	}
	return mode
}
