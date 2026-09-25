package logging

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestFileLoggingLevelsAndLegacyLogger(t *testing.T) {
	for _, test := range []struct {
		level string
		debug bool
		info  bool
		warn  bool
	}{
		{level: "", debug: true, info: true, warn: true},
		{level: " DEBUG ", debug: true, info: true, warn: true},
		{level: "info", info: true, warn: true},
		{level: "warn", warn: true},
		{level: "error"},
	} {
		t.Run("level="+test.level, func(t *testing.T) {
			directory := t.TempDir()
			stderr, err := os.Create(filepath.Join(directory, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			path := filepath.Join(directory, "logs", "porto.log")
			previous := slog.Default()
			closeLog, err := Open(path, test.level, stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer closeLog()
			slog.Debug("synthetic debug")
			slog.Info("synthetic info")
			slog.Warn("synthetic warning")
			slog.Error("synthetic error")
			log.Print("synthetic legacy log")
			if err := closeLog(); err != nil {
				t.Fatal(err)
			}
			if slog.Default() != previous {
				t.Fatal("closing the log did not restore the previous logger")
			}
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for message, want := range map[string]bool{
				"synthetic debug": test.debug, "synthetic info": test.info,
				"synthetic warning": test.warn, "synthetic error": true,
				"synthetic legacy log": test.info,
			} {
				if got := strings.Contains(string(contents), message); got != want {
					t.Errorf("contains %q = %t, want %t: %s", message, got, want, contents)
				}
			}
			console, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(console) != string(contents) {
				t.Fatal("foreground daemon diagnostics were not mirrored to stderr")
			}
			if runtime.GOOS != "windows" {
				for name, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
					info, err := os.Stat(name)
					if err != nil {
						t.Fatal(err)
					}
					if info.Mode().Perm() != want {
						t.Errorf("%s permissions = %o, want %o", name, info.Mode().Perm(), want)
					}
				}
			}
		})
	}
}

func TestFileLoggingAvoidsDuplicatingRedirectedStderrAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.log")
	stderr, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	for _, message := range []string{"first synthetic launch", "second synthetic launch"} {
		closeLog, err := Open(path, "", stderr)
		if err != nil {
			t.Fatal(err)
		}
		slog.Debug(message)
		if err := closeLog(); err != nil {
			t.Fatal(err)
		}
		if err := closeLog(); err != nil {
			t.Fatalf("closing twice: %v", err)
		}
	}
	if _, err := stderr.WriteString("raw child stderr remains open\n"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"first synthetic launch", "second synthetic launch", "raw child stderr remains open"} {
		if count := strings.Count(string(contents), message); count != 1 {
			t.Errorf("%q appears %d times, want exactly once: %s", message, count, contents)
		}
	}
}

func TestFileLoggingRejectsInvalidConfiguration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "logs", "porto.log")
	if _, err := Open(path, "verbose", os.Stderr); err == nil || !strings.Contains(err.Error(), LevelEnv) {
		t.Fatalf("invalid level = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid level created a log: %v", err)
	}
	if err := os.WriteFile(filepath.Dir(path), []byte("synthetic obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, "debug", os.Stderr); err == nil || !strings.Contains(err.Error(), "log directory") {
		t.Fatalf("invalid log directory = %v", err)
	}
}

func TestFileWriterReportsWriteFailure(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Create(filepath.Join(directory, "closed.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	writer := &fileWriter{file: file, stderr: stderr, mirror: true}
	if _, err := writer.Write([]byte("original synthetic diagnostic\n")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("write failure = %v", err)
	}
	contents, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"write log file", "original synthetic diagnostic"} {
		if !strings.Contains(string(contents), message) {
			t.Errorf("stderr is missing %q: %s", message, contents)
		}
	}
}

func TestFileLoggingCapturesFatalRuntimeOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "porto.log")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := process.NewCommand(ctx, "", os.Args[0], "-test.run=^TestLogCrashHelper$")
	command.Env = process.WithEnvironment(os.Environ(), "PORTO_TEST_LOG_CRASH="+path)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err == nil {
		t.Fatal("crash helper unexpectedly succeeded")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "panic: synthetic logging crash") {
		t.Fatalf("fatal runtime output was lost: %s", contents)
	}
}

func TestLogCrashHelper(t *testing.T) {
	path := os.Getenv("PORTO_TEST_LOG_CRASH")
	if path == "" {
		return
	}
	if _, err := Open(path, "", os.Stderr); err != nil {
		t.Fatal(err)
	}
	go func() { panic("synthetic logging crash") }()
	select {}
}
