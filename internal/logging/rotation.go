package logging

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"
)

const dateLayout = "2006-01-02"

func (s *Session) SetRetentionDays(days int) error {
	if days < 1 {
		return errors.New("log retention days must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retentionDays = days
	s.retentionReady = true
	return s.maintainLocked(s.now())
}

func (s *Session) Maintain() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintainLocked(s.now())
}

func (s *Session) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Maintain(); err != nil {
				_, _ = fmt.Fprintf(s.stderr, "porto: maintain log file %s: %v\n", s.path, err)
			}
		}
	}
}

// maintainLocked rotates a stale or oversized live log, then compresses,
// expires and caps archives. Writes only rotate; this full pass runs on
// rotation, at startup and from the maintenance ticker.
func (s *Session) maintainLocked(now time.Time) error {
	_, err := s.rotateIfNeededLocked(now, 0)
	return errors.Join(err, s.cleanupLocked(now))
}

// rotateIfNeededLocked rotates at the calendar-day boundary, or before an
// append of incoming bytes would push the live file past its size cap.
func (s *Session) rotateIfNeededLocked(now time.Time, incoming int64) (bool, error) {
	rotated, err := s.rotateLocked(now)
	if err != nil {
		return rotated, err
	}
	if s.maxFileBytes <= 0 || s.file == nil {
		return rotated, nil
	}
	info, err := s.file.Stat()
	if err != nil {
		return rotated, fmt.Errorf("inspect active Porto log size: %w", err)
	}
	if info.Size() == 0 || info.Size()+incoming <= s.maxFileBytes {
		return rotated, nil
	}
	segmentPath, err := s.nextSegmentPath(s.activeDate)
	if err != nil {
		return rotated, err
	}
	if err := s.archiveActiveLocked(now, segmentPath); err != nil {
		return rotated, err
	}
	return true, nil
}

func (s *Session) cleanupLocked(now time.Time) error {
	var result error
	if err := s.compressStableLogsLocked(now); err != nil {
		result = errors.Join(result, err)
	}
	if s.retentionReady {
		if err := s.pruneArchivesLocked(now); err != nil {
			result = errors.Join(result, err)
		}
	}
	if err := s.enforceDirectoryCapLocked(now); err != nil {
		result = errors.Join(result, err)
	}
	if s.stateDirty {
		if err := s.writeActiveDateLocked(s.activeDate); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (s *Session) initializeActiveDateLocked(now time.Time) error {
	statePath := s.statePath()
	data, err := os.ReadFile(statePath)
	if err == nil {
		value := strings.TrimSpace(string(data))
		if _, parseErr := time.ParseInLocation(dateLayout, value, now.Location()); parseErr != nil {
			return fmt.Errorf("parse Porto log date state %s: %w", statePath, parseErr)
		}
		s.activeDate = value
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read Porto log date state %s: %w", statePath, err)
	}
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect Porto log file date: %w", err)
	}
	s.activeDate = now.Format(dateLayout)
	if info.Size() > 0 {
		s.activeDate = info.ModTime().In(now.Location()).Format(dateLayout)
	}
	return s.writeActiveDateLocked(s.activeDate)
}

// rotateLocked archives the previous day's live log at the calendar-day
// boundary. The day's final part keeps the undecorated porto-<date> name; if
// it already exists, an earlier rotation was interrupted after renaming and the
// live file holds current-day records, so it is kept.
func (s *Session) rotateLocked(now time.Time) (bool, error) {
	today := now.Format(dateLayout)
	if s.activeDate == today {
		return false, nil
	}
	info, err := s.file.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect active Porto log before rotation: %w", err)
	}
	rawPath := s.rawLogPath(s.activeDate)
	completed, err := anyExists(rawPath, s.zipPath(rawPath))
	if err != nil {
		return false, fmt.Errorf("inspect rotated Porto log %s: %w", rawPath, err)
	}
	if completed || info.Size() == 0 {
		s.activeDate = today
		s.stateDirty = true
		return false, nil
	}
	if err := s.archiveActiveLocked(now, rawPath); err != nil {
		return false, err
	}
	s.activeDate = today
	s.stateDirty = true
	return true, nil
}

func (s *Session) archiveActiveLocked(now time.Time, rawPath string) error {
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync Porto log before rotation: %w", err)
	}
	if s.crashOutput {
		if err := debug.SetCrashOutput(nil, debug.CrashOptions{}); err != nil {
			return fmt.Errorf("pause Porto crash logging for rotation: %w", err)
		}
	}
	file := s.file
	s.file = nil
	if err := file.Close(); err != nil {
		reopenErr := s.reopenActiveLogLocked()
		return errors.Join(fmt.Errorf("close Porto log for rotation: %w", err), reopenErr)
	}
	if err := s.renameActiveLog(rawPath); err != nil {
		reopenErr := s.reopenActiveLogLocked()
		return errors.Join(fmt.Errorf("rotate Porto log to %s: %w", rawPath, err), reopenErr)
	}
	if err := os.Chtimes(rawPath, now, now); err != nil {
		_, _ = fmt.Fprintf(s.stderr, "porto: mark rotated log %s for compression: %v\n", rawPath, err)
	}
	if err := s.reopenActiveLogLocked(); err != nil {
		rollbackErr := os.Rename(rawPath, s.path)
		reopenRollbackErr := s.reopenActiveLogLocked()
		return errors.Join(fmt.Errorf("open new Porto log after rotation: %w", err), rollbackErr, reopenRollbackErr)
	}
	return nil
}

// nextSegmentPath names a size rotation within a day: porto-<date>.<n>.log,
// numbered in write order and never reusing a raw or compressed name.
func (s *Session) nextSegmentPath(date string) (string, error) {
	for sequence := 1; ; sequence++ {
		rawPath := filepath.Join(filepath.Dir(s.path), fmt.Sprintf("%s-%s.%d.log", s.logStem(), date, sequence))
		taken, err := anyExists(rawPath, s.zipPath(rawPath))
		if err != nil {
			return "", fmt.Errorf("inspect rotated Porto log %s: %w", rawPath, err)
		}
		if !taken {
			return rawPath, nil
		}
	}
}

func anyExists(paths ...string) (bool, error) {
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (s *Session) renameActiveLog(rawPath string) error {
	var err error
	for attempt := 0; attempt < s.renameAttempts; attempt++ {
		err = os.Rename(s.path, rawPath)
		if err == nil {
			return nil
		}
		if attempt+1 < s.renameAttempts {
			time.Sleep(s.renameDelay)
		}
	}
	return err
}

func (s *Session) reopenActiveLogLocked() error {
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		return errors.Join(err, file.Close())
	}
	if s.crashOutput {
		if err := debug.SetCrashOutput(file, debug.CrashOptions{}); err != nil {
			return errors.Join(err, file.Close())
		}
	}
	s.file = file
	return nil
}

func (s *Session) compressStableLogsLocked(now time.Time) error {
	segments, err := s.segments(".log")
	if err != nil {
		return err
	}
	var result error
	for _, segment := range segments {
		info, statErr := os.Lstat(segment.path)
		if statErr != nil {
			result = errors.Join(result, fmt.Errorf("inspect rotated Porto log %s: %w", segment.path, statErr))
			continue
		}
		if !info.Mode().IsRegular() || now.Sub(info.ModTime()) < s.archiveDelay {
			continue
		}
		if archiveErr := zipLog(segment.path, s.zipPath(segment.path), info); archiveErr != nil {
			result = errors.Join(result, archiveErr)
			continue
		}
		if removeErr := os.Remove(segment.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove compressed Porto log %s: %w", segment.path, removeErr))
		}
	}
	return result
}

func zipLog(rawPath, archivePath string, rawInfo os.FileInfo) error {
	entryName := strings.TrimSuffix(filepath.Base(rawPath), filepath.Ext(rawPath)) + ".log"
	if matches, err := archiveMatches(archivePath, entryName, rawInfo.Size()); err == nil && matches {
		return nil
	}
	input, err := os.Open(rawPath)
	if err != nil {
		return fmt.Errorf("open rotated Porto log %s: %w", rawPath, err)
	}
	defer input.Close()
	tempPath := fmt.Sprintf("%s.tmp-%d", archivePath, os.Getpid())
	output, err := os.OpenFile(tempPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create Porto log archive %s: %w", tempPath, err)
	}
	if err := output.Chmod(0o600); err != nil {
		_ = output.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("protect Porto log archive %s: %w", tempPath, err)
	}
	archive := zip.NewWriter(output)
	header, err := zip.FileInfoHeader(rawInfo)
	if err == nil {
		header.Name = entryName
		header.Method = zip.Deflate
		header.Modified = rawInfo.ModTime()
		var entry io.Writer
		entry, err = archive.CreateHeader(header)
		if err == nil {
			_, err = io.CopyN(entry, input, rawInfo.Size())
		}
	}
	closeErr := archive.Close()
	syncErr := output.Sync()
	fileCloseErr := output.Close()
	if err = errors.Join(err, closeErr, syncErr, fileCloseErr); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("write Porto log archive %s: %w", archivePath, err)
	}
	if err := os.Remove(archivePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace Porto log archive %s: %w", archivePath, err)
	}
	if err := os.Rename(tempPath, archivePath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("publish Porto log archive %s: %w", archivePath, err)
	}
	return nil
}

func archiveMatches(archivePath, entryName string, size int64) (bool, error) {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer archive.Close()
	return len(archive.File) == 1 &&
		archive.File[0].Name == entryName &&
		int64(archive.File[0].UncompressedSize64) == size, nil
}

// pruneArchivesLocked deletes compressed and uncompressed archives dated
// before the retention window, which counts calendar days including today.
func (s *Session) pruneArchivesLocked(now time.Time) error {
	cutoff := startOfDay(now).AddDate(0, 0, -(s.retentionDays - 1))
	var result error
	for _, extension := range []string{".zip", ".log"} {
		segments, err := s.segments(extension)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, segment := range segments {
			date, parseErr := time.ParseInLocation(dateLayout, segment.date, now.Location())
			if parseErr != nil || !date.Before(cutoff) {
				continue
			}
			if removeErr := removeRegularFile(segment.path); removeErr != nil {
				result = errors.Join(result, fmt.Errorf("remove expired Porto log archive %s: %w", segment.path, removeErr))
			}
		}
	}
	return result
}

// enforceDirectoryCapLocked keeps the live log plus archives within the total
// cap. It reserves the live file's maximum size and deletes the oldest
// archives first, even inside the retention window, then records what it removed.
func (s *Session) enforceDirectoryCapLocked(now time.Time) error {
	if s.maxTotalBytes <= 0 {
		return nil
	}
	var archives []logSegment
	for _, extension := range []string{".zip", ".log"} {
		segments, err := s.segments(extension)
		if err != nil {
			return err
		}
		archives = append(archives, segments...)
	}
	var used int64
	for index := range archives {
		info, err := os.Lstat(archives[index].path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		archives[index].size = info.Size()
		used += info.Size()
	}
	budget := s.maxTotalBytes - s.maxFileBytes
	if used <= budget {
		return nil
	}
	sort.Slice(archives, func(left, right int) bool { return archives[left].before(archives[right]) })
	var result error
	removed, freed := 0, int64(0)
	oldestKept := ""
	for _, archive := range archives {
		if used <= budget {
			oldestKept = filepath.Base(archive.path)
			break
		}
		if archive.size == 0 {
			continue
		}
		if err := removeRegularFile(archive.path); err != nil {
			result = errors.Join(result, fmt.Errorf("remove Porto log archive %s over the size cap: %w", archive.path, err))
			continue
		}
		used -= archive.size
		freed += archive.size
		removed++
	}
	if removed > 0 {
		s.recordLocked(now, slog.LevelWarn, "Deleted diagnostic log archives to stay under the size cap",
			"removed", removed, "freedBytes", freed, "capBytes", s.maxTotalBytes, "oldestKept", oldestKept)
	}
	return result
}

// recordLocked writes a maintenance record straight to the live file, since
// logging through the session while it holds its lock would deadlock.
func (s *Session) recordLocked(now time.Time, level slog.Level, message string, args ...any) {
	output := io.Writer(s.file)
	if s.file == nil {
		output = s.stderr
	} else if s.mirror {
		output = io.MultiWriter(s.file, s.stderr)
	}
	record := slog.NewRecord(now, level, message, 0)
	record.Add(append([]any{"component", "daemon", "pid", os.Getpid()}, args...)...)
	_ = slog.NewTextHandler(output, nil).Handle(context.Background(), record)
}

type logSegment struct {
	path     string
	date     string
	sequence int
	size     int64
}

// before orders a day's size segments (1..n) ahead of its final daily part.
func (left logSegment) before(right logSegment) bool {
	if left.date != right.date {
		return left.date < right.date
	}
	leftSequence, rightSequence := left.sequence, right.sequence
	if leftSequence == 0 {
		leftSequence = int(^uint(0) >> 1)
	}
	if rightSequence == 0 {
		rightSequence = int(^uint(0) >> 1)
	}
	return leftSequence < rightSequence
}

// segments lists owned archives with the extension: porto-<date><ext> and
// porto-<date>.<n><ext>. Unrelated names, directories and symlinks are skipped.
func (s *Session) segments(extension string) ([]logSegment, error) {
	pattern := filepath.Join(filepath.Dir(s.path), s.logStem()+"-????-??-??*"+extension)
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("list Porto log archives: %w", err)
	}
	segments := make([]logSegment, 0, len(paths))
	for _, path := range paths {
		if segment, ok := s.parseSegment(path, extension); ok {
			segments = append(segments, segment)
		}
	}
	return segments, nil
}

func (s *Session) parseSegment(path, extension string) (logSegment, bool) {
	name := filepath.Base(path)
	prefix := s.logStem() + "-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) {
		return logSegment{}, false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(name, prefix), extension)
	date, sequenceValue, segmented := strings.Cut(value, ".")
	if _, err := time.Parse(dateLayout, date); err != nil {
		return logSegment{}, false
	}
	segment := logSegment{path: path, date: date}
	if segmented {
		sequence, err := strconv.Atoi(sequenceValue)
		if err != nil || sequence < 1 || strconv.Itoa(sequence) != sequenceValue {
			return logSegment{}, false
		}
		segment.sequence = sequence
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return logSegment{}, false
	}
	return segment, true
}

func removeRegularFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeOrphanedTemporaryFiles deletes archive and state files left half
// written by a crashed process. Their sources stay and are retried.
func (s *Session) removeOrphanedTemporaryFiles() error {
	directory := filepath.Dir(s.path)
	var result error
	for _, pattern := range []string{s.logStem() + "-????-??-??*.zip.tmp-*", ".porto-log-date.tmp-*"} {
		paths, err := filepath.Glob(filepath.Join(directory, pattern))
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, path := range paths {
			_, pid, _ := strings.Cut(filepath.Base(path), ".tmp-")
			if _, err := strconv.Atoi(pid); err != nil {
				continue
			}
			if err := removeRegularFile(path); err != nil {
				result = errors.Join(result, fmt.Errorf("remove interrupted Porto log file %s: %w", path, err))
			}
		}
	}
	return result
}

func (s *Session) writeActiveDateLocked(date string) error {
	statePath := s.statePath()
	tempPath := fmt.Sprintf("%s.tmp-%d", statePath, os.Getpid())
	if err := os.WriteFile(tempPath, []byte(date+"\n"), 0o600); err != nil {
		return fmt.Errorf("write Porto log date state %s: %w", tempPath, err)
	}
	if err := os.Chmod(tempPath, 0o600); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("protect Porto log date state %s: %w", tempPath, err)
	}
	if err := os.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replace Porto log date state %s: %w", statePath, err)
	}
	if err := os.Rename(tempPath, statePath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("publish Porto log date state %s: %w", statePath, err)
	}
	s.stateDirty = false
	return nil
}

func (s *Session) statePath() string {
	return filepath.Join(filepath.Dir(s.path), ".porto-log-date")
}

func (s *Session) rawLogPath(date string) string {
	return filepath.Join(filepath.Dir(s.path), s.logStem()+"-"+date+".log")
}

func (s *Session) zipPath(rawPath string) string {
	return strings.TrimSuffix(rawPath, ".log") + ".zip"
}

func (s *Session) logStem() string {
	name := filepath.Base(s.path)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func startOfDay(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}
