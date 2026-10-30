package gitutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if lockPath := os.Getenv("PORTO_TEST_INDEX_LOCK"); lockPath != "" && filepath.Base(os.Args[0]) == "git" {
		if os.Getenv("LC_ALL") != "C" || os.Getenv("LANG") != "C" || len(os.Args) < 2 {
			fmt.Fprintln(os.Stderr, "synthetic Git requires normalized locale and a command")
			os.Exit(2)
		}
		switch os.Args[1] {
		case "rev-parse":
			fmt.Println(lockPath)
			os.Exit(0)
		case "pull":
			if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			time.Sleep(time.Minute)
			os.Exit(0)
		default:
			fmt.Fprintln(os.Stderr, "unexpected synthetic Git command")
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

func TestGitTimeoutRemovesLockCreatedByPortoProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group test")
	}
	repository := t.TempDir()
	lockPath := filepath.Join(repository, "index.lock")
	binDirectory := t.TempDir()
	gitPath := filepath.Join(binDirectory, "git")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the running test executable rather than cold-starting a new script under the Git deadlines.
	if err := os.Symlink(executable, gitPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_TEST_INDEX_LOCK", lockPath)
	t.Setenv("PATH", binDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))

	output, err := gitWithTimeout(repository, 10*time.Second, "pull", "--ff-only")
	if err == nil {
		t.Fatal("timed-out Git command unexpectedly succeeded")
	}
	if !strings.Contains(output, "Porto removed its stale Git index lock") {
		t.Fatalf("cleanup message missing: %q (Git error: %v)", output, err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Porto-owned lock still exists: %v", err)
	}
}

func TestGitIndexLockPathHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := gitIndexLockPath(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("index lock discovery error = %v, want context cancellation", err)
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
