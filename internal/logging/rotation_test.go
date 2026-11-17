package logging

import (
	"archive/zip"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func openTestSession(t *testing.T, directory string, now time.Time, maxFileBytes, maxTotalBytes int64) *Session {
	t.Helper()
	stderr, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stderr.Close() })
	diagnostics, err := open(filepath.Join(directory, "porto.log"), "", stderr, openOptions{
		now:            func() time.Time { return now },
		archiveDelay:   0,
		renameAttempts: 1,
		maxFileBytes:   maxFileBytes,
		maxTotalBytes:  maxTotalBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { diagnostics.Close() })
	return diagnostics
}

func readZipEntry(t *testing.T, archivePath string) (string, string) {
	t.Helper()
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if len(archive.File) != 1 {
		t.Fatalf("%s entries = %d", archivePath, len(archive.File))
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	contents, err := io.ReadAll(entry)
	if err != nil {
		t.Fatal(err)
	}
	return archive.File[0].Name, string(contents)
}

func TestSizeRotationArchivesNumberedSegmentsWithinTheCap(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	diagnostics := openTestSession(t, directory, now, 100, 10_000)
	records := []string{strings.Repeat("a", 59) + "\n", strings.Repeat("b", 39) + "\n", strings.Repeat("c", 59) + "\n", strings.Repeat("d", 59) + "\n"}
	for _, record := range records {
		if _, err := diagnostics.Write([]byte(record)); err != nil {
			t.Fatal(err)
		}
	}
	// 60+40 bytes fill the 100 byte cap exactly; each later record rotates first.
	for sequence, want := range map[int]string{1: records[0] + records[1], 2: records[2]} {
		name, contents := readZipEntry(t, filepath.Join(directory, "porto-2026-09-29."+strconv.Itoa(sequence)+".zip"))
		if name != "porto-2026-09-29."+strconv.Itoa(sequence)+".log" || contents != want {
			t.Fatalf("segment %d = %s %q", sequence, name, contents)
		}
	}
	current, err := os.ReadFile(filepath.Join(directory, "porto.log"))
	if err != nil || string(current) != records[3] {
		t.Fatalf("live log = %q (%v)", current, err)
	}
}

func TestOversizedRecordIsTruncatedWithMarker(t *testing.T) {
	directory := t.TempDir()
	diagnostics := openTestSession(t, directory, time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC), 80, 10_000)
	record := []byte(strings.Repeat("é", 60) + "\n")
	if n, err := diagnostics.Write(record); err != nil || n != len(record) {
		t.Fatalf("write = %d, %v", n, err)
	}
	current, err := os.ReadFile(filepath.Join(directory, "porto.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(current) > 80 || !utf8.Valid(current) || !strings.Contains(string(current), "[truncated 121 bytes to the 80 byte log file cap]") {
		t.Fatalf("bounded record = %q (%d bytes)", current, len(current))
	}
}

func TestDirectoryCapDeletesOldestArchivesAndRecordsIt(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"porto-2026-09-27.zip", "porto-2026-09-28.1.zip", "porto-2026-09-28.zip"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(strings.Repeat("x", 100)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(directory, "notes.txt")
	if err := os.WriteFile(unrelated, []byte(strings.Repeat("y", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := openTestSession(t, directory, now, 100, 300)
	if err := diagnostics.SetRetentionDays(7); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{"porto-2026-09-27.zip": false, "porto-2026-09-28.1.zip": true, "porto-2026-09-28.zip": true, "notes.txt": true} {
		if _, err := os.Stat(filepath.Join(directory, name)); (err == nil) != kept {
			t.Errorf("%s kept=%v, want %v (%v)", name, err == nil, kept, err)
		}
	}
	current, err := os.ReadFile(filepath.Join(directory, "porto.log"))
	if err != nil || !strings.Contains(string(current), `level=WARN msg="Deleted diagnostic log archives to stay under the size cap"`) ||
		!strings.Contains(string(current), "removed=1 freedBytes=100") || !strings.Contains(string(current), "oldestKept=porto-2026-09-28.1.zip") {
		t.Fatalf("cap record = %q (%v)", current, err)
	}
}

func TestRetentionRemovesExpiredRawSegmentsButNotLinkedOrUnrelatedFiles(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "kept.log")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"porto-2026-09-01.3.log", "porto-2026-09-01.zip.tmp-123", ".porto-log-date.tmp-456", "porto-2026-09-01.zip.tmp-x", "other-2026-09-01.log"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(directory, "porto-2026-09-02.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	diagnostics := openTestSession(t, directory, time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC), 10_000, 100_000)
	if err := diagnostics.SetRetentionDays(7); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{
		"porto-2026-09-01.3.log": false, "porto-2026-09-01.zip.tmp-123": false, ".porto-log-date.tmp-456": false,
		"porto-2026-09-01.zip.tmp-x": true, "other-2026-09-01.log": true, "porto-2026-09-02.log": true,
	} {
		if _, err := os.Lstat(filepath.Join(directory, name)); (err == nil) != kept {
			t.Errorf("%s kept=%v, want %v (%v)", name, err == nil, kept, err)
		}
	}
	if contents, err := os.ReadFile(outside); err != nil || string(contents) != "outside" {
		t.Fatalf("symlink target = %q (%v)", contents, err)
	}
}

func TestSizeLimitsFromEnvironmentValidatesSettings(t *testing.T) {
	environment := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	file, total, err := SizeLimitsFromEnvironment(environment(nil))
	if err != nil || file != 10*mebibyte || total != 50*mebibyte {
		t.Fatalf("defaults = %d %d %v", file, total, err)
	}
	file, total, err = SizeLimitsFromEnvironment(environment(map[string]string{MaxFileEnv: "2", MaxTotalEnv: "200"}))
	if err != nil || file != 2*mebibyte || total != 200*mebibyte {
		t.Fatalf("overrides = %d %d %v", file, total, err)
	}
	for _, values := range []map[string]string{
		{MaxFileEnv: "0"}, {MaxTotalEnv: "unlimited"}, {MaxFileEnv: "-1"}, {MaxFileEnv: "50", MaxTotalEnv: "50"},
	} {
		if _, _, err := SizeLimitsFromEnvironment(environment(values)); err == nil {
			t.Errorf("accepted invalid limits %v", values)
		}
	}
}
