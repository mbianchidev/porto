package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func withLimaGuestDiagnostics(cause error) error {
	if errors.Is(cause, context.Canceled) {
		return cause
	}
	home := os.Getenv("LIMA_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return errors.Join(cause, fmt.Errorf("locate Porto guest logs: %w", err))
		}
		home = filepath.Join(userHome, ".lima")
	}
	logPath := filepath.Join(home, engineInstanceName, "serial.log")
	diagnostic, err := readLimaGuestPanic(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return cause
	}
	if err != nil {
		return errors.Join(cause, fmt.Errorf("inspect Porto guest serial log: %w", err))
	}
	if diagnostic == "" {
		return cause
	}
	return fmt.Errorf("Porto guest serial log reports %s (see %s): %w", diagnostic, logPath, cause)
}

func readLimaGuestPanic(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular log file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	const limit = 64 * 1024
	data := make([]byte, min(info.Size(), limit))
	n, err := file.ReadAt(data, max(0, info.Size()-limit))
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	var diagnostic string
	for _, line := range strings.Split(string(data[:n]), "\n") {
		if strings.Contains(line, "Linux version ") {
			diagnostic = ""
		}
		if index := strings.Index(line, "Kernel panic - not syncing:"); index >= 0 &&
			!strings.Contains(line[:index], "end ") {
			diagnostic = strings.TrimSpace(line[index:])
			diagnostic = diagnostic[:min(len(diagnostic), 512)]
		}
	}
	return diagnostic, nil
}
