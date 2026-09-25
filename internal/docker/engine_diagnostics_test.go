package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestOwnershipFailureReportsGuestPanic(t *testing.T) {
	const panicLine = "Kernel panic - not syncing: synthetic init failure"
	for _, test := range []struct {
		name      string
		log       string
		commandOK bool
		canceled  bool
		wantPanic bool
	}{
		{name: "current boot panic", log: "[0.0] Linux version synthetic\n[42.0] " + panicLine + "\n", wantPanic: true},
		{name: "panic after large log", log: strings.Repeat("unrelated guest output\n", 10000) + panicLine + "\n", wantPanic: true},
		{name: "bounded panic message", log: panicLine + strings.Repeat(" detail", 1000) + "\n", wantPanic: true},
		{name: "panic end marker", log: panicLine + "\ntrace details\n---[ end " + panicLine + " ]---\n", wantPanic: true},
		{name: "old panic before reboot", log: panicLine + "\n[0.0] Linux version synthetic\nstarting services\n"},
		{name: "old panic outside bounded tail", log: panicLine + "\n" + strings.Repeat("boot progress\n", 10000)},
		{name: "no panic", log: "starting guest services\n"},
		{name: "missing log"},
		{name: "healthy guest ignores old log", log: panicLine + "\n", commandOK: true},
		{name: "caller cancellation", log: panicLine + "\n", canceled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("LIMA_HOME", home)
			logPath := filepath.Join(home, engineInstanceName, "serial.log")
			if test.log != "" {
				if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(logPath, []byte(test.log), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			commandErr := context.DeadlineExceeded
			if test.canceled {
				commandErr = context.Canceled
			}
			runner := &fakeRunner{handler: func(runtimes.Command) ([]byte, error) {
				if test.commandOK {
					return []byte("synthetic-owner\n"), nil
				}
				return []byte("synthetic SSH diagnostic"), commandErr
			}}
			err := New(runner).verifyLimaOwnership(context.Background(), "synthetic-owner")
			if test.commandOK {
				if err != nil {
					t.Fatalf("healthy ownership check: %v", err)
				}
				return
			}
			if !errors.Is(err, commandErr) || !strings.Contains(err.Error(), "synthetic SSH diagnostic") {
				t.Fatalf("lost command failure: %v", err)
			}
			if strings.Contains(err.Error(), panicLine) != test.wantPanic {
				t.Fatalf("panic diagnostic = %v, want panic %t", err, test.wantPanic)
			}
			if test.wantPanic && !strings.Contains(err.Error(), logPath) {
				t.Fatalf("missing guest log path: %v", err)
			}
			if strings.Contains(err.Error(), "unrelated guest output") || len(err.Error()) > 2048 {
				t.Fatalf("guest diagnostic was not bounded: %d bytes", len(err.Error()))
			}
			for _, command := range runner.commands {
				if command.Name != "limactl" || command.Args[0] != "shell" {
					t.Fatalf("diagnosis modified guest lifecycle: %+v", command)
				}
			}
		})
	}
}
