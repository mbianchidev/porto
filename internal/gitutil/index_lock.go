package gitutil

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

type indexLockTracker struct {
	path        string
	preexisting os.FileInfo
	observed    os.FileInfo
	stop        chan struct{}
	done        chan struct{}
	mu          sync.Mutex
}

func startIndexLockTracker(path string) *indexLockTracker {
	tracker := &indexLockTracker{
		path: path,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if info, err := os.Stat(path); err == nil {
		tracker.preexisting = info
	}
	go tracker.watch()
	return tracker
}

func (t *indexLockTracker) watch() {
	defer close(t.done)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		t.observe()
		select {
		case <-ticker.C:
		case <-t.stop:
			t.observe()
			return
		}
	}
}

func (t *indexLockTracker) observe() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.preexisting != nil || t.observed != nil {
		return
	}
	if info, err := os.Stat(t.path); err == nil {
		t.observed = info
	}
}

func (t *indexLockTracker) cleanup(output string) (bool, error) {
	close(t.stop)
	<-t.done
	if t.preexisting != nil || gitLockCollision(output) {
		return false, nil
	}
	t.mu.Lock()
	observed := t.observed
	t.mu.Unlock()
	if observed == nil {
		return false, nil
	}
	current, err := os.Stat(t.path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(observed, current) {
		return false, nil
	}
	if err := os.Remove(t.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func gitLockCollision(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "index.lock") &&
		(strings.Contains(output, "file exists") || strings.Contains(output, "another git process"))
}
