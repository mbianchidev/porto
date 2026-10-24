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

func TestWindowsSharePathsKeepNativeDrivesAndConfineLinuxRoots(t *testing.T) {
	for _, test := range []struct {
		name, linuxPath, probedPath, want string
		invalid                           bool
	}{
		{"native-drive", "/mnt/c/Users/Synthetic User", `C:\Users\Synthetic User`, `C:\Users\Synthetic User`, false},
		{"custom-automount", "/windows/c/Dev", "c:/Dev's space", `C:\Dev's space`, false},
		{"selected-linux", "/home/synthetic", `\\wsl.localhost\other\home\synthetic`, `\\wsl.localhost\selected\home\synthetic`, false},
		{"relative-drive", "/mnt/c/Dev", `C:Dev`, "", true},
		{"invalid-drive", "/mnt/c/Dev", `1:\Dev`, "", true},
		{"control", "/mnt/c/Dev", "C:\\Dev\n", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := windowsSharePath("selected", test.linuxPath, test.probedPath)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("Windows share = %q, %v; want %q, invalid=%t", got, err, test.want, test.invalid)
			}
		})
	}
}
