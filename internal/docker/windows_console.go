package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (m *Manager) validateNativeExec(request ExecRequest) error {
	if !m.IsNativeWindows() {
		return nil
	}
	if _, err := windowsEnvironment(nil, request.Environment); err != nil {
		return err
	}
	if len(request.Command) == 0 || strings.TrimSpace(request.Command[0]) == "" {
		return errors.New("invalid native Windows exec command")
	}
	if request.Privileged {
		return fmt.Errorf("%w: native Windows privileged exec", ErrUnsupported)
	}
	if request.TTY {
		if err := runtimes.ValidatePTYSize(request.ConsoleSize); err != nil {
			return err
		}
	}
	if request.WorkingDir != "" && !windowsPath(request.WorkingDir) {
		return errors.New("invalid native Windows exec working directory")
	}
	for _, value := range append(append([]string(nil), request.Environment...), request.User) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid native Windows exec option")
		}
	}
	for _, value := range request.Command {
		if strings.ContainsRune(value, 0) {
			return errors.New("invalid native Windows exec command")
		}
	}
	return nil
}

func (m *Manager) resizeWindowsConsole(ctx context.Context, id string, w, h uint32) error {
	if w == 0 || h == 0 || w > runtimes.MaxPTYDimension || h > runtimes.MaxPTYDimension {
		return errors.New("invalid terminal dimensions")
	}
	driver, err := m.nativeWindowsDriver(ctx)
	if err != nil {
		return err
	}
	return driver.Console(ctx, id, [2]uint{uint(h), uint(w)}, true)
}
