package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var limaKernelRecord = regexp.MustCompile(`^\[\s*\d+(?:\.\d+)?\]`)

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
	lines := strings.Split(string(data[:n]), "\n")
	boot, panicIndex := 0, -1
	for index, line := range lines {
		if strings.Contains(line, "Linux version ") {
			boot, panicIndex = index, -1
		}
		if offset := strings.Index(line, "Kernel panic - not syncing:"); offset >= 0 &&
			!strings.Contains(line[:offset], "end ") {
			panicIndex = index
		}
	}
	if panicIndex < 0 {
		return "", nil
	}
	line := lines[panicIndex]
	diagnostic := strings.TrimSpace(line[strings.Index(line, "Kernel panic - not syncing:"):])
	diagnostic = strings.ToValidUTF8(diagnostic[:min(len(diagnostic), 512)], "")
	var records strings.Builder
	for _, line := range lines[max(boot, panicIndex-12):min(len(lines), panicIndex+24)] {
		line = strings.TrimSpace(line)
		if !limaKernelRecord.MatchString(line) || strings.Contains(line, "Kernel panic - not syncing:") ||
			strings.Contains(line, "Linux version ") {
			continue
		}
		records.WriteString(line)
		records.WriteByte('\n')
		if records.Len() >= 1024 {
			break
		}
	}
	if records.Len() > 0 {
		details := records.String()
		diagnostic += "\nGuest panic context:\n" + strings.ToValidUTF8(details[:min(len(details), 1024)], "")
	}
	return diagnostic, nil
}
