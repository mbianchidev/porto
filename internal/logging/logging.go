package logging

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
)

const LevelEnv = "PORTO_LOG_LEVEL"

// Open routes process-wide diagnostics until the returned closer is called.
func Open(path, levelName string, stderr *os.File) (func() error, error) {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(levelName)) {
	case "", "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("%s must be debug, info, warn, or error, got %q", LevelEnv, levelName)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create Porto log directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect Porto log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Porto log file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(fmt.Errorf("protect Porto log file: %w", err), file.Close())
	}
	info, err := file.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("inspect Porto log file: %w", err), file.Close())
	}
	stderrInfo, stderrErr := stderr.Stat()
	redirected := stderrErr == nil && os.SameFile(info, stderrInfo)
	if !redirected {
		if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
			return nil, errors.Join(fmt.Errorf("capture Porto crash output: %w", err), file.Close())
		}
	}
	previousLogger := slog.Default()
	previousOutput, previousFlags, previousPrefix := log.Writer(), log.Flags(), log.Prefix()
	writer := &fileWriter{file: file, stderr: stderr, mirror: stderrErr == nil && !redirected}
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: level})).
		With("component", "daemon", "pid", os.Getpid())
	slog.SetDefault(logger)
	if stderrErr != nil {
		slog.Debug("Standard error unavailable; writing diagnostics to the log file", "error", stderrErr)
	}

	var once sync.Once
	var closeErr error
	return func() error {
		once.Do(func() {
			slog.SetDefault(previousLogger)
			log.SetOutput(previousOutput)
			log.SetFlags(previousFlags)
			log.SetPrefix(previousPrefix)
			if !redirected {
				closeErr = debug.SetCrashOutput(nil, debug.CrashOptions{})
			}
			closeErr = errors.Join(closeErr, file.Close())
		})
		return closeErr
	}, nil
}

type fileWriter struct {
	file   *os.File
	stderr *os.File
	mirror bool
}

func (w *fileWriter) Write(data []byte) (int, error) {
	n, err := w.file.Write(data)
	if err != nil {
		_, reportErr := fmt.Fprintf(w.stderr, "porto: write log file %s: %v\n", w.file.Name(), err)
		err = errors.Join(err, reportErr)
	}
	if w.mirror {
		_, mirrorErr := w.stderr.Write(data)
		err = errors.Join(err, mirrorErr)
	}
	return n, err
}
