package logging

import (
	"archive/zip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDailyLogRotationCreatesZipAndKeepsCurrentLog(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "porto.log")
	stderr, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	location := time.FixedZone("synthetic", 2*60*60)
	now := time.Date(2026, time.September, 29, 0, 5, 0, 0, location)
	if err := os.WriteFile(logPath, []byte("previous day\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := now.AddDate(0, 0, -1)
	if err := os.Chtimes(logPath, previous, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".porto-log-date"), []byte("2026-09-28\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := open(logPath, "", stderr, openOptions{
		now:            func() time.Time { return now },
		archiveDelay:   0,
		renameAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	if err := diagnostics.SetRetentionDays(7); err != nil {
		t.Fatal(err)
	}
	slog.Info("current day")
	archivePath := filepath.Join(directory, "porto-2026-09-28.zip")
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "porto-2026-09-28.log" {
		archive.Close()
		t.Fatalf("archive entries = %+v", archive.File)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		archive.Close()
		t.Fatal(err)
	}
	archived, err := io.ReadAll(entry)
	closeErr := entry.Close()
	archiveErr := archive.Close()
	if err != nil || closeErr != nil || archiveErr != nil {
		t.Fatal(err, closeErr, archiveErr)
	}
	if string(archived) != "previous day\n" {
		t.Fatalf("archived log = %q", archived)
	}
	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "previous day") || !strings.Contains(string(current), "current day") {
		t.Fatalf("current log = %s", current)
	}
	state, err := os.ReadFile(filepath.Join(directory, ".porto-log-date"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(state)) != "2026-09-29" {
		t.Fatalf("active date = %q", state)
	}
	if _, err := os.Stat(filepath.Join(directory, "porto-2026-09-28.log")); !os.IsNotExist(err) {
		t.Fatalf("raw rotated log still exists: %v", err)
	}
}

func TestLogRetentionKeepsConfiguredCalendarDays(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "porto.log")
	stderr, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	for _, date := range []string{"2026-09-21", "2026-09-22", "2026-09-23", "2026-09-28"} {
		if err := os.WriteFile(filepath.Join(directory, "porto-"+date+".zip"), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := open(logPath, "", stderr, openOptions{
		now:            func() time.Time { return now },
		archiveDelay:   0,
		renameAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	if err := diagnostics.SetRetentionDays(7); err != nil {
		t.Fatal(err)
	}
	for _, date := range []string{"2026-09-21", "2026-09-22"} {
		if _, err := os.Stat(filepath.Join(directory, "porto-"+date+".zip")); !os.IsNotExist(err) {
			t.Errorf("expired archive %s was not deleted: %v", date, err)
		}
	}
	for _, date := range []string{"2026-09-23", "2026-09-28"} {
		if _, err := os.Stat(filepath.Join(directory, "porto-"+date+".zip")); err != nil {
			t.Errorf("retained archive %s is unavailable: %v", date, err)
		}
	}
}

func TestRotationRecoveryDoesNotOverwriteCompletedDay(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "porto.log")
	rawPath := filepath.Join(directory, "porto-2026-09-28.log")
	stderr, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	now := time.Date(2026, time.September, 29, 0, 5, 0, 0, time.UTC)
	if err := os.WriteFile(logPath, []byte("current day after interrupted rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, []byte("completed previous day\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := now.AddDate(0, 0, -1)
	if err := os.Chtimes(rawPath, previous, previous); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".porto-log-date"), []byte("2026-09-28\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := open(logPath, "", stderr, openOptions{
		now:            func() time.Time { return now },
		archiveDelay:   0,
		renameAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostics.Close()
	if err := diagnostics.Maintain(); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "current day after interrupted rotation\n" {
		t.Fatalf("current log was replaced: %q", current)
	}
	archive, err := zip.OpenReader(filepath.Join(directory, "porto-2026-09-28.zip"))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		archive.Close()
		t.Fatal(err)
	}
	contents, err := io.ReadAll(entry)
	closeErr := entry.Close()
	archiveErr := archive.Close()
	if err != nil || closeErr != nil || archiveErr != nil {
		t.Fatal(err, closeErr, archiveErr)
	}
	if string(contents) != "completed previous day\n" {
		t.Fatalf("completed archive was overwritten: %q", contents)
	}
}
