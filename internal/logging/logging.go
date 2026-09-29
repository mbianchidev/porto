package logging

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	LevelEnv        = "PORTO_LOG_LEVEL"
	MirrorStderrEnv = "PORTO_LOG_MIRROR_STDERR"
)

type Session struct {
	mu             sync.Mutex
	path           string
	file           *os.File
	stderr         *os.File
	mirror         bool
	crashOutput    bool
	activeDate     string
	stateDirty     bool
	retentionDays  int
	retentionReady bool
	now            func() time.Time
	archiveDelay   time.Duration
	renameDelay    time.Duration
	renameAttempts int
	previousLogger *slog.Logger
	previousOutput io.Writer
	previousFlags  int
	previousPrefix string
	closeOnce      sync.Once
	closeErr       error
}

type openOptions struct {
	now            func() time.Time
	archiveDelay   time.Duration
	renameDelay    time.Duration
	renameAttempts int
}

// Open routes process-wide diagnostics until the returned session is closed.
func Open(path, levelName string, stderr *os.File) (*Session, error) {
	return open(path, levelName, stderr, openOptions{
		now:            time.Now,
		archiveDelay:   time.Minute,
		renameDelay:    25 * time.Millisecond,
		renameAttempts: 80,
	})
}

func open(path, levelName string, stderr *os.File, options openOptions) (*Session, error) {
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
	if options.now == nil {
		options.now = time.Now
	}
	if options.renameAttempts <= 0 {
		options.renameAttempts = 1
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
	session := &Session{
		path:           path,
		file:           file,
		stderr:         stderr,
		mirror:         stderrErr == nil && !redirected && !mirrorStderrDisabled(),
		crashOutput:    !redirected,
		now:            options.now,
		archiveDelay:   options.archiveDelay,
		renameDelay:    options.renameDelay,
		renameAttempts: options.renameAttempts,
		previousLogger: previousLogger,
		previousOutput: previousOutput,
		previousFlags:  previousFlags,
		previousPrefix: previousPrefix,
	}
	if err := session.initializeActiveDateLocked(options.now()); err != nil {
		if !redirected {
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		}
		return nil, errors.Join(err, file.Close())
	}
	logger := slog.New(slog.NewTextHandler(session, &slog.HandlerOptions{Level: level})).
		With("component", "daemon", "pid", os.Getpid())
	slog.SetDefault(logger)
	if stderrErr != nil {
		slog.Debug("Standard error unavailable; writing diagnostics to the log file", "error", stderrErr)
	}
	return session, nil
}

func mirrorStderrDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(MirrorStderrEnv))) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		slog.SetDefault(s.previousLogger)
		log.SetOutput(s.previousOutput)
		log.SetFlags(s.previousFlags)
		log.SetPrefix(s.previousPrefix)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.crashOutput {
			s.closeErr = debug.SetCrashOutput(nil, debug.CrashOptions{})
		}
		if s.file != nil {
			s.closeErr = errors.Join(s.closeErr, s.file.Close())
			s.file = nil
		}
	})
	return s.closeErr
}

func (s *Session) Write(data []byte) (int, error) {
	s.mu.Lock()
	if s.file == nil {
		s.mu.Unlock()
		return 0, os.ErrClosed
	}
	maintenanceErr := s.maintainLocked(s.now())
	if s.file == nil {
		s.mu.Unlock()
		_, reportErr := fmt.Fprintf(s.stderr, "porto: maintain log file %s: %v\n", s.path, maintenanceErr)
		return 0, errors.Join(os.ErrClosed, maintenanceErr, reportErr)
	}
	n, err := s.file.Write(data)
	s.mu.Unlock()
	if maintenanceErr != nil {
		_, reportErr := fmt.Fprintf(s.stderr, "porto: maintain log file %s: %v\n", s.path, maintenanceErr)
		err = errors.Join(err, reportErr)
	}
	if err != nil {
		_, reportErr := fmt.Fprintf(s.stderr, "porto: write log file %s: %v\n", s.path, err)
		err = errors.Join(err, reportErr)
	}
	if s.mirror {
		_, mirrorErr := s.stderr.Write(data)
		err = errors.Join(err, mirrorErr)
	}
	return n, err
}
