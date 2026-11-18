//go:build darwin

package docker

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type launchDaemonInstaller struct {
	path      string
	launchctl func(args ...string) ([]byte, error)
}

var endpointLaunchDaemons = launchDaemonInstaller{path: EndpointLaunchDaemonPath, launchctl: runLaunchctl}

func runLaunchctl(args ...string) ([]byte, error) {
	return exec.Command("/bin/launchctl", args...).CombinedOutput()
}

func installEndpointLaunchDaemon(canonicalPath, targetPath string) error {
	return endpointLaunchDaemons.install(canonicalPath, targetPath)
}

func removeEndpointLaunchDaemon() error {
	return endpointLaunchDaemons.remove()
}

// EndpointPersisted reports whether the boot-time job that restores the canonical endpoint is installed.
func EndpointPersisted() bool {
	_, err := os.Stat(endpointLaunchDaemons.path)
	return err == nil
}

func (d launchDaemonInstaller) install(canonicalPath, targetPath string) error {
	data, err := endpointLaunchDaemon(canonicalPath, targetPath)
	if err != nil {
		return err
	}
	if err := d.unload(); err != nil {
		return err
	}
	if err := writeLaunchDaemon(d.path, data); err != nil {
		return err
	}
	if output, err := d.launchctl("bootstrap", "system", d.path); err != nil {
		removeErr := os.Remove(d.path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		return errors.Join(launchctlError("load", err, output), removeErr)
	}
	return nil
}

func (d launchDaemonInstaller) remove() error {
	if err := d.unload(); err != nil {
		return err
	}
	if err := os.Remove(d.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", d.path, err)
	}
	return nil
}

func (d launchDaemonInstaller) unload() error {
	if _, err := d.launchctl("print", "system/"+EndpointLaunchDaemonLabel); err != nil {
		return nil
	}
	if output, err := d.launchctl("bootout", "system/"+EndpointLaunchDaemonLabel); err != nil {
		return launchctlError("unload", err, output)
	}
	return nil
}

func launchctlError(action string, err error, output []byte) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("%s %s: %w", action, EndpointLaunchDaemonLabel, err)
	}
	return fmt.Errorf("%s %s: %w: %s", action, EndpointLaunchDaemonLabel, err, detail)
}

// writeLaunchDaemon installs the plist atomically with the root-owned, non-writable mode launchd requires.
func writeLaunchDaemon(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return fmt.Errorf("protect %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("install %s: %w", path, err)
	}
	return nil
}
