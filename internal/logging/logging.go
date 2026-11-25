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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/porto-containers/porto/internal/app"
)

const (
	LevelEnv        = "PORTO_LOG_LEVEL"
	MirrorStderrEnv = "PORTO_LOG_MIRROR_STDERR"
	// MaxFileEnv caps the live log in MB before it rotates within a day. Like
	// MaxTotalEnv, it overrides the limits stored in Settings.
	MaxFileEnv = "PORTO_LOG_MAX_FILE_MB"
	// MaxTotalEnv caps the live log plus its archives in MB.
	MaxTotalEnv = "PORTO_LOG_MAX_TOTAL_MB"
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
	maxFileBytes   int64
	maxTotalBytes  int64
	// limitsFromEnvironment keeps environment caps ahead of stored settings.
	limitsFromEnvironment bool
	now                   func() time.Time
	archiveDelay          time.Duration
	renameDelay           time.Duration
	renameAttempts        int
	previousLogger        *slog.Logger
	previousOutput        io.Writer
	previousFlags         int
	previousPrefix        string
	closeOnce             sync.Once
	closeErr              error
}

type openOptions struct {
	now            func() time.Time
	archiveDelay   time.Duration
	renameDelay    time.Duration
	renameAttempts int
	maxFileBytes   int64
	maxTotalBytes  int64
	// limitsFromEnvironment marks caps that stored settings must not replace.
	limitsFromEnvironment bool
}

// Open routes process-wide diagnostics until the returned session is closed.
func Open(path, levelName string, stderr *os.File) (*Session, error) {
	fileMB, totalMB, fromEnvironment, err := SizeLimitsFromEnvironment(os.Getenv)
	if err != nil {
		return nil, err
	}
	return open(path, levelName, stderr, openOptions{
		now:                   time.Now,
		archiveDelay:          time.Minute,
		renameDelay:           25 * time.Millisecond,
		renameAttempts:        80,
		maxFileBytes:          int64(fileMB) * app.LogMegabyte,
		maxTotalBytes:         int64(totalMB) * app.LogMegabyte,
		limitsFromEnvironment: fromEnvironment,
	})
}

// SizeLimitsFromEnvironment resolves the live-file and total log caps in MB,
// reporting whether either environment variable set them. Unset variables use
// the defaults until stored settings are applied.
func SizeLimitsFromEnvironment(getenv func(string) string) (fileMB, totalMB int, fromEnvironment bool, err error) {
	fileMB, fileSet, err := megabytesSetting(getenv, MaxFileEnv, app.DefaultLogMaxFileMB)
	if err != nil {
		return 0, 0, false, err
	}
	totalMB, totalSet, err := megabytesSetting(getenv, MaxTotalEnv, app.DefaultLogMaxTotalMB)
	if err != nil {
		return 0, 0, false, err
	}
	if err := app.ValidateLogSizeLimits(fileMB, totalMB); err != nil {
		return 0, 0, false, fmt.Errorf("%s/%s: %w", MaxFileEnv, MaxTotalEnv, err)
	}
	return fileMB, totalMB, fileSet || totalSet, nil
}

func megabytesSetting(getenv func(string) string, name string, fallback int) (int, bool, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return fallback, false, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > app.MaximumLogSizeMB {
		return 0, false, fmt.Errorf("%s must be a whole number of MB from 1 to %d, got %q", name, app.MaximumLogSizeMB, value)
	}
	return parsed, true, nil
}

// SizeLimitsFromEnvironment reports whether environment variables fixed this
// session's caps, in which case stored settings do not change them.
func (s *Session) SizeLimitsFromEnvironment() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limitsFromEnvironment
}

// SetSizeLimitsMB applies stored size limits and immediately rotates or prunes
// to honor them. It keeps environment overrides in place.
func (s *Session) SetSizeLimitsMB(fileMB, totalMB int) error {
	if err := app.ValidateLogSizeLimits(fileMB, totalMB); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limitsFromEnvironment {
		return nil
	}
	s.maxFileBytes = int64(fileMB) * app.LogMegabyte
	s.maxTotalBytes = int64(totalMB) * app.LogMegabyte
	if s.file == nil {
		return nil
	}
	return s.maintainLocked(s.now())
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
	if options.maxFileBytes <= 0 {
		options.maxFileBytes = app.DefaultLogMaxFileMB * app.LogMegabyte
	}
	if options.maxTotalBytes <= 0 {
		options.maxTotalBytes = app.DefaultLogMaxTotalMB * app.LogMegabyte
	}
	if options.maxTotalBytes <= options.maxFileBytes {
		return nil, errors.Join(fmt.Errorf("total log cap %d bytes must exceed the %d byte live file cap",
			options.maxTotalBytes, options.maxFileBytes), file.Close())
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
		path:                  path,
		file:                  file,
		stderr:                stderr,
		mirror:                stderrErr == nil && !redirected && !mirrorStderrDisabled(),
		crashOutput:           !redirected,
		now:                   options.now,
		archiveDelay:          options.archiveDelay,
		renameDelay:           options.renameDelay,
		renameAttempts:        options.renameAttempts,
		maxFileBytes:          options.maxFileBytes,
		maxTotalBytes:         options.maxTotalBytes,
		previousLogger:        previousLogger,
		limitsFromEnvironment: options.limitsFromEnvironment,
		previousOutput:        previousOutput,
		previousFlags:         previousFlags,
		previousPrefix:        previousPrefix,
	}
	if err := session.initializeActiveDateLocked(options.now()); err != nil {
		if !redirected {
			_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
		}
		return nil, errors.Join(err, file.Close())
	}
	if err := session.removeOrphanedTemporaryFiles(); err != nil {
		_, _ = fmt.Fprintf(stderr, "porto: clean interrupted log files in %s: %v\n", directory, err)
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
	original := data
	s.mu.Lock()
	if s.file == nil {
		s.mu.Unlock()
		return 0, os.ErrClosed
	}
	now := s.now()
	data = s.boundRecord(data)
	rotated, maintenanceErr := s.rotateIfNeededLocked(now, int64(len(data)))
	if rotated {
		maintenanceErr = errors.Join(maintenanceErr, s.cleanupLocked(now))
	}
	if s.file == nil {
		s.mu.Unlock()
		_, reportErr := fmt.Fprintf(s.stderr, "porto: maintain log file %s: %v\n", s.path, maintenanceErr)
		return 0, errors.Join(os.ErrClosed, maintenanceErr, reportErr)
	}
	n, err := s.file.Write(data)
	s.mu.Unlock()
	if n == len(data) {
		// A truncated record was written in full as far as the caller is concerned.
		n = len(original)
	}
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

// boundRecord truncates a single record that cannot fit in the live file cap,
// on a UTF-8 boundary and with an explicit marker, so no file exceeds the cap.
func (s *Session) boundRecord(data []byte) []byte {
	if s.maxFileBytes <= 0 || int64(len(data)) <= s.maxFileBytes {
		return data
	}
	marker := fmt.Sprintf(" [truncated %d bytes to the %d byte log file cap]\n", len(data), s.maxFileBytes)
	keep := int(s.maxFileBytes) - len(marker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(data[keep]) {
		keep--
	}
	bounded := make([]byte, 0, keep+len(marker))
	bounded = append(bounded, data[:keep]...)
	return append(bounded, marker...)
}
