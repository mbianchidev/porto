package runtimes

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}
	if runtime.GOOS != "darwin" || strings.ContainsRune(name, filepath.Separator) {
		return "", err
	}
	path, fallbackErr := lookPathInDirectories(name, []string{"/opt/homebrew/bin", "/usr/local/bin"})
	if fallbackErr == nil {
		return path, nil
	}
	return "", errors.Join(err, fallbackErr)
}

func ResolveQEMUPath(
	command string,
	lookPath func(string) (string, error),
	getenv func(string) string,
) (string, error) {
	overrideName := ""
	switch command {
	case "qemu-system-aarch64":
		overrideName = "QEMU_SYSTEM_AARCH64"
	case "qemu-system-x86_64":
		overrideName = "QEMU_SYSTEM_X86_64"
	}
	if overrideName != "" {
		if override := strings.TrimSpace(getenv(overrideName)); override != "" {
			path, err := lookPath(override)
			if err != nil {
				return "", fmt.Errorf("%s points to an unavailable QEMU executable: %w", overrideName, err)
			}
			return path, nil
		}
	}
	return lookPath(command)
}

func lookPathInDirectories(name string, directories []string) (string, error) {
	for _, directory := range directories {
		candidate := filepath.Join(directory, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s was not found in fallback executable directories", name)
}
