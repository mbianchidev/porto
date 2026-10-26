package wsl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func authorizationFixture(t *testing.T) (*Manager, Integration) {
	t.Helper()
	manager := New(t.TempDir(), &rejectingRunner{})
	manager.platform = "windows"
	integration := Integration{Distribution: "selected", ID: "synthetic-id", Owner: strings.Repeat("a", 32)}
	root := "/home/synthetic"
	sum := sha256.Sum256([]byte(integration.ID + "\x00" + root))
	integration.Shares = []Share{{
		LinuxPath: root, WindowsPath: `\\wsl.localhost\selected\home\synthetic`,
		GuestPath: "/mnt/porto-wsl/" + hex.EncodeToString(sum[:8]),
	}}
	manager.discover = func(context.Context) ([]Distribution, error) {
		return []Distribution{{Name: integration.Distribution, ID: integration.ID, Version: 2, Running: true}}, nil
	}
	if err := manager.save([]Integration{integration}); err != nil {
		t.Fatal(err)
	}
	return manager, integration
}

func TestWSLAuthorizationWaitsForOnlyTheSelectedFilesystemToReconnect(t *testing.T) {
	manager, integration := authorizationFixture(t)
	calls := 0
	manager.probeShare = func(root string) error {
		calls++
		if root != integration.Shares[0].WindowsPath {
			t.Fatal("readiness touched an unselected filesystem")
		}
		if calls < 3 {
			return &os.PathError{Op: "CreateFile", Path: root, Err: syscall.Errno(1)}
		}
		return nil
	}
	if _, err := manager.Authorize(context.Background(), integration.Distribution, integration.Owner); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("request was admitted before the selected provider became ready: calls=%d", calls)
	}
}

func TestWSLFilesystemReadinessPreservesCancellationAndPermissionErrors(t *testing.T) {
	for _, scenario := range []string{"canceled", "denied", "unselected"} {
		t.Run(scenario, func(t *testing.T) {
			manager, integration := authorizationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			manager.probeShare = func(string) error {
				calls++
				if scenario == "canceled" {
					cancel()
					return syscall.Errno(1)
				}
				return syscall.Errno(5)
			}
			owner := integration.Owner
			if scenario == "unselected" {
				owner = strings.Repeat("b", 32)
			}
			_, err := manager.Authorize(ctx, integration.Distribution, owner)
			if err == nil {
				t.Fatal("an unavailable or unauthorized filesystem was admitted")
			}
			if scenario == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("client cancellation was lost: %v", err)
			}
			if scenario == "denied" && (!errors.Is(err, syscall.Errno(5)) || calls != 1) {
				t.Fatalf("a permanent permission error was retried: calls=%d, error=%v", calls, err)
			}
			if scenario == "unselected" && calls != 0 {
				t.Fatal("an unauthorized request probed a filesystem")
			}
		})
	}
}

func TestWSLStoppedStatusNeverProbesOrStartsItsFilesystem(t *testing.T) {
	manager, integration := authorizationFixture(t)
	manager.discover = func(context.Context) ([]Distribution, error) {
		return []Distribution{{Name: integration.Distribution, ID: integration.ID, Version: 2}}, nil
	}
	manager.probeShare = func(string) error {
		t.Fatal("read-only stopped status touched a WSL filesystem")
		return nil
	}
	if _, err := manager.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Authorize(context.Background(), integration.Distribution, integration.Owner); err == nil {
		t.Fatal("a stopped distribution was implicitly started for a request")
	}
}

func TestSelectedShareReadinessRequiresAnActuallyReadableDirectory(t *testing.T) {
	root := t.TempDir()
	if err := probeSharedDirectory(root); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "synthetic-file")
	if err := os.WriteFile(file, []byte("synthetic fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := probeSharedDirectory(root); err != nil {
		t.Fatal(err)
	}
	if err := probeSharedDirectory(file); err == nil {
		t.Fatal("a replacement regular file was accepted as a readable share directory")
	}
}
