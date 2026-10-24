package wsl

import (
	"context"
	"errors"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

type rejectingRunner struct{ calls int }

func (r *rejectingRunner) Run(context.Context, runtimes.Command) ([]byte, error) {
	r.calls++
	return nil, errors.New("unexpected distribution mutation")
}

func TestEnableRejectsUnselectedOrWSL1DistributionsBeforeMutation(t *testing.T) {
	runner := &rejectingRunner{}
	manager := New(t.TempDir(), runner)
	manager.platform = "windows"
	manager.discover = func(context.Context) ([]Distribution, error) {
		return []Distribution{{Name: "selected", ID: "synthetic-id", Version: 1}}, nil
	}
	for _, name := range []string{"unselected", "selected", "--distribution", "bad\nname"} {
		if _, err := manager.Enable(context.Background(), name, nil); err == nil {
			t.Errorf("enabled unsupported distribution %q", name)
		}
	}
	if runner.calls != 0 {
		t.Fatalf("invalid selection ran %d commands", runner.calls)
	}
}
