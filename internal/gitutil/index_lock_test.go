package gitutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGitTimeoutRemovesLockCreatedByPortoProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based process-group test")
	}
	repository := t.TempDir()
	lockPath := filepath.Join(repository, "index.lock")
	binDirectory := t.TempDir()
	gitPath := filepath.Join(binDirectory, "git")
	script := `#!/bin/sh
	if [ "$LC_ALL" != "C" ] || [ "$LANG" != "C" ]; then
	  exit 2
	fi
	if [ "$1" = "rev-parse" ]; then
  printf '%s\n' "$PORTO_TEST_INDEX_LOCK"
  exit 0
fi
: > "$PORTO_TEST_INDEX_LOCK"
sleep 10
`
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_TEST_INDEX_LOCK", lockPath)
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	output, err := gitWithTimeout(repository, time.Second, "pull", "--ff-only")
	if err == nil {
		t.Fatal("timed-out Git command unexpectedly succeeded")
	}
	if !strings.Contains(output, "Porto removed its stale Git index lock") {
		t.Fatalf("cleanup message missing: %q", output)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Porto-owned lock still exists: %v", err)
	}
}

func TestIndexLockCleanupPreservesPreexistingLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "index.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tracker := startIndexLockTracker(lockPath)
	removed, err := tracker.cleanup("")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("preexisting lock was removed")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("preexisting lock was not preserved: %v", err)
	}
}

func TestIndexLockCleanupPreservesCollisionLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "index.lock")
	tracker := startIndexLockTracker(lockPath)
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitForObservedIndexLock(t, tracker)
	removed, err := tracker.cleanup(
		"Unable to create '.git/index.lock': File exists. Another git process seems to be running.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("lock from a reported Git collision was removed")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("collision lock was not preserved: %v", err)
	}
}

func waitForObservedIndexLock(t *testing.T, tracker *indexLockTracker) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tracker.mu.Lock()
		observed := tracker.observed != nil
		tracker.mu.Unlock()
		if observed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("index lock was not observed")
}
