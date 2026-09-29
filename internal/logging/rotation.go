package logging

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
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
	return s.pruneArchivesLocked(s.now())
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

func (s *Session) maintainLocked(now time.Time) error {
	var result error
	if err := s.rotateLocked(now); err != nil {
		result = errors.Join(result, err)
	}
	if err := s.compressStableLogsLocked(now); err != nil {
		result = errors.Join(result, err)
	}
	if s.retentionReady {
		if err := s.pruneArchivesLocked(now); err != nil {
			result = errors.Join(result, err)
		}
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

func (s *Session) rotateLocked(now time.Time) error {
	today := now.Format(dateLayout)
	if s.activeDate == today {
		return nil
	}
	rawPath := s.rawLogPath(s.activeDate)
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect active Porto log before rotation: %w", err)
	}
	if _, err := os.Stat(rawPath); err == nil {
		s.activeDate = today
		s.stateDirty = true
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect rotated Porto log %s: %w", rawPath, err)
	}
	if info.Size() == 0 {
		s.activeDate = today
		s.stateDirty = true
		return nil
	}
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
	s.activeDate = today
	s.stateDirty = true
	return nil
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
	pattern := filepath.Join(filepath.Dir(s.path), s.logStem()+"-????-??-??.log")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("list rotated Porto logs: %w", err)
	}
	var result error
	for _, rawPath := range files {
		date, ok := s.dateFromPath(rawPath, ".log")
		if !ok || date == now.Format(dateLayout) {
			continue
		}
		info, statErr := os.Stat(rawPath)
		if statErr != nil {
			result = errors.Join(result, fmt.Errorf("inspect rotated Porto log %s: %w", rawPath, statErr))
			continue
		}
		if now.Sub(info.ModTime()) < s.archiveDelay {
			continue
		}
		if archiveErr := zipLog(rawPath, s.archivePath(date), info); archiveErr != nil {
			result = errors.Join(result, archiveErr)
			continue
		}
		if removeErr := os.Remove(rawPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove compressed Porto log %s: %w", rawPath, removeErr))
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

func (s *Session) pruneArchivesLocked(now time.Time) error {
	pattern := filepath.Join(filepath.Dir(s.path), s.logStem()+"-????-??-??.zip")
	files, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("list Porto log archives: %w", err)
	}
	cutoff := startOfDay(now).AddDate(0, 0, -(s.retentionDays - 1))
	var result error
	for _, archivePath := range files {
		dateValue, ok := s.dateFromPath(archivePath, ".zip")
		if !ok {
			continue
		}
		date, parseErr := time.ParseInLocation(dateLayout, dateValue, now.Location())
		if parseErr != nil || !date.Before(cutoff) {
			continue
		}
		if removeErr := os.Remove(archivePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove expired Porto log archive %s: %w", archivePath, removeErr))
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

func (s *Session) archivePath(date string) string {
	return filepath.Join(filepath.Dir(s.path), s.logStem()+"-"+date+".zip")
}

func (s *Session) logStem() string {
	name := filepath.Base(s.path)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func (s *Session) dateFromPath(path, extension string) (string, bool) {
	name := filepath.Base(path)
	prefix := s.logStem() + "-"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, extension) {
		return "", false
	}
	date := strings.TrimSuffix(strings.TrimPrefix(name, prefix), extension)
	_, err := time.Parse(dateLayout, date)
	return date, err == nil
}

func startOfDay(value time.Time) time.Time {
	year, month, day := value.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, value.Location())
}
