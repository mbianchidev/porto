package nativefiles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
	"github.com/mbianchidev/porto/internal/runtimefiles"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type fixtureRunner struct{}

func (fixtureRunner) Run(context.Context, runtimes.Command) ([]byte, error) {
	return nil, errors.New("synthetic unavailable driver")
}

func TestNativeAttachmentRejectsImagesWritesAndStaleIdentities(t *testing.T) {
	manager := New(t.TempDir(), fixtureRunner{})
	image := datafiles.Resource{Kind: "image", Name: "fixture-image", ID: "fixture-digest", ReadOnly: true}
	target := Target{Resource: image, Identity: image.Fingerprint()}
	if _, err := manager.Attach(context.Background(), target, true); !errors.Is(err, datafiles.ErrUnsupported) {
		t.Fatalf("image writes were accepted: %v", err)
	}
	target.Identity = "reused-name"
	if _, err := manager.Attach(context.Background(), target, false); !errors.Is(err, datafiles.ErrConflict) {
		t.Fatalf("stale identity was accepted: %v", err)
	}
}

func TestNativeLocationsAreCollisionSafeForUnicodeAndRecreatedResources(t *testing.T) {
	first := datafiles.Resource{Kind: "volume", Name: "fixture-\u03b4", ID: "source-a"}
	second := first
	second.ID = "source-b"
	if attachmentID(first, false) == attachmentID(second, false) || attachmentID(first, true) == attachmentID(first, false) {
		t.Fatal("resource identity or write boundary did not change its native location")
	}
}

func TestNativeUnavailableCapabilityProvidesAnActionableAlternative(t *testing.T) {
	manager := New(t.TempDir(), fixtureRunner{})
	manager.lookPath = func(string) (string, error) { return "", errors.New("synthetic missing SSHFS") }
	capability := manager.Capability(context.Background(), false)
	if capability.Supported || capability.Message == "" || capability.Fallback == "" {
		t.Fatalf("unsafe success-shaped capability: %+v", capability)
	}
}

type nativeRunnerFunc func(context.Context, runtimes.Command) ([]byte, error)

func (run nativeRunnerFunc) Run(ctx context.Context, command runtimes.Command) ([]byte, error) {
	return run(ctx, command)
}

func TestNativeDisconnectedBridgeNeverReturnsAnEmptyDirectoryAsConnected(t *testing.T) {
	manager := New(t.TempDir(), nativeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) {
		return nil, nil
	}))
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	id := attachmentID(resource, false)
	record := Attachment{
		ID: id, Resource: resource, Identity: resource.Fingerprint(),
		Path: filepath.Join(manager.root, id), ReadOnly: true, State: "connected",
	}
	if err := os.Mkdir(record.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	manager.attachments[id] = &liveAttachment{
		record: record,
		target: Target{Verify: func(context.Context) error { return nil }},
	}
	disconnected, err := manager.Get(context.Background(), id)
	if err == nil || disconnected.State != "disconnected" {
		t.Fatalf("an empty directory was reported as a live native filesystem: %+v, %v", disconnected, err)
	}
	if len(manager.List()) != 1 {
		t.Fatal("the disconnected attachment disappeared before owned cleanup")
	}
}

func TestNativeReplacedMountIsNeverOpenedOrDetached(t *testing.T) {
	root := t.TempDir()
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	id := attachmentID(resource, false)
	record := Attachment{
		ID: id, Resource: resource, Identity: resource.Fingerprint(),
		Path: filepath.Join(root, id), ReadOnly: true, State: "connected",
	}
	unmounted, released := false, false
	manager := New(root, nativeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		if command.Name == "mount" {
			return []byte("foreign-filesystem on " + record.Path + " type fuse.sshfs (ro)\n"), nil
		}
		unmounted = true
		return nil, nil
	}))
	manager.attachments[id] = &liveAttachment{
		record: record,
		target: Target{
			Verify: func(context.Context) error { return nil },
			Release: func(context.Context) error {
				released = true
				return nil
			},
		},
	}
	if _, err := manager.Get(context.Background(), id); !errors.Is(err, datafiles.ErrConflict) {
		t.Fatalf("a replaced host mount was exposed: %v", err)
	}
	if err := manager.Detach(context.Background(), id); !errors.Is(err, datafiles.ErrConflict) || unmounted || released {
		t.Fatalf("cleanup touched an unowned filesystem: err=%v unmounted=%v released=%v", err, unmounted, released)
	}
	remaining := manager.List()
	if len(remaining) != 1 || remaining[0].State != "detach-failed" {
		t.Fatalf("failed owned detach was hidden: %+v", remaining)
	}
}

type nativeStartupRunner struct {
	nativeRunnerFunc
	start func(context.Context, runtimes.Command) (runtimes.Process, error)
}

func (runner nativeStartupRunner) Start(ctx context.Context, command runtimes.Command) (runtimes.Process, error) {
	return runner.start(ctx, command)
}

func TestNativeStartupReservationAndFailedCleanupRemainRecoverable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSHFS startup lifecycle; Windows uses its installed WinFsp fixture")
	}
	for _, failRelease := range []bool{false, true} {
		t.Run(fmt.Sprintf("release-fails-%t", failRelease), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "synthetic native \u03b4")
			resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
			id := attachmentID(resource, false)
			startupError := errors.New("synthetic bridge startup failure")
			reserved := false
			runner := nativeStartupRunner{
				nativeRunnerFunc: func(_ context.Context, command runtimes.Command) ([]byte, error) {
					if command.Name == "synthetic-sshfs" {
						return []byte("contain_symlinks"), nil
					}
					return nil, nil
				},
				start: func(_ context.Context, command runtimes.Command) (runtimes.Process, error) {
					document, err := os.ReadFile(filepath.Join(root, id+".json"))
					reserved = err == nil && strings.Contains(string(document), `"state":"connecting"`)
					if !strings.Contains(strings.Join(command.Args, ","), "fsname=porto-"+id) {
						t.Error("the bridge has no exact owned mount identity")
					}
					return nil, startupError
				},
			}
			manager := New(root, runner)
			manager.lookPath = func(string) (string, error) { return "synthetic-sshfs", nil }
			releases := 0
			target := Target{
				Resource: resource, Identity: resource.Fingerprint(), RemotePath: "/synthetic",
				Backend:   &runtimefiles.Descriptor{Resource: resource, Owner: "synthetic-owner"},
				Token:     id,
				SSHConfig: filepath.Join(root, "synthetic.config"), SSHHost: "synthetic-host",
				Verify: func(context.Context) error { return nil },
				Release: func(ctx context.Context) error {
					releases++
					if ctx.Err() != nil {
						t.Error("cleanup inherited the cancelled startup context")
					}
					if failRelease {
						return os.ErrPermission
					}
					return nil
				},
			}
			_, err := manager.Attach(context.Background(), target, false)
			if !errors.Is(err, startupError) || !reserved || releases != 1 {
				t.Fatalf("startup was not durably owned and cleaned: err=%v reserved=%v releases=%d", err, reserved, releases)
			}
			if failRelease {
				remaining := manager.List()
				if !errors.Is(err, os.ErrPermission) || len(remaining) != 1 || remaining[0].State != "detach-failed" {
					t.Fatalf("failed backend cleanup lost its recovery record: %+v, %v", remaining, err)
				}
				failRelease = false
				restarted := New(root, runner)
				recovered := false
				if err := restarted.Recover(context.Background(), func(_ context.Context, record Attachment) error {
					recovered = record.Identity == target.Identity && record.Token == id && record.Backend.Owner == "synthetic-owner"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if !recovered || len(restarted.List()) != 0 {
					t.Fatal("daemon restart did not recover only its exact backend reservation")
				}
			}
			if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatalf("cleaned startup left native resources behind: %v, %v", entries, err)
			}
		})
	}
}

func TestNativeMountOwnershipHandlesEscapedAndNonASCIIPaths(t *testing.T) {
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-\u03b4", ID: "volume-1"}
	id := attachmentID(resource, false)
	record := Attachment{ID: id, Path: "/synthetic native \u03b4 (fixture) type directory/" + id}
	for _, linux := range []bool{false, true} {
		mountpoint, suffix := record.Path, " (macfuse, read-only)"
		if linux {
			mountpoint = strings.ReplaceAll(mountpoint, " ", `\040`)
			suffix = " type fuse.sshfs (ro,nosuid,nodev)"
		}
		manager := New(t.TempDir(), nativeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) {
			return []byte("porto-" + id + " on " + mountpoint + suffix + "\n"), nil
		}))
		mounted, err := manager.mounted(context.Background(), record)
		if err != nil || !mounted {
			t.Fatalf("owned mount path was not recognized: linux=%v, %v", linux, err)
		}
	}
}

func TestNativePartialDetachDoesNotRepeatBackendReleaseOrDeleteUnexpectedFiles(t *testing.T) {
	root := t.TempDir()
	resource := datafiles.Resource{Kind: "volume", Name: "synthetic-volume", ID: "volume-1"}
	id := attachmentID(resource, false)
	record := Attachment{
		ID: id, Identity: resource.Fingerprint(), Resource: resource, Path: filepath.Join(root, id),
		State: "connected", ReadOnly: true, Backend: &runtimefiles.Descriptor{Resource: resource}, Token: id,
	}
	if err := os.Mkdir(record.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(record.Path, "synthetic-unexpected-file")
	if err := os.WriteFile(unexpected, []byte("synthetic host contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := nativeRunnerFunc(func(context.Context, runtimes.Command) ([]byte, error) { return nil, nil })
	manager := New(root, runner)
	releases := 0
	manager.attachments[id] = &liveAttachment{record: record, target: Target{
		Release: func(context.Context) error { releases++; return nil },
	}}
	if err := manager.Detach(context.Background(), id); err == nil || releases != 1 {
		t.Fatalf("unexpected host content was removed or backend release failed: %v, releases=%d", err, releases)
	}
	restarted := New(root, runner)
	if err := restarted.Recover(context.Background(), func(context.Context, Attachment) error {
		releases++
		return errors.New("released backend must not be released again")
	}); err == nil {
		t.Fatal("recovery removed unexpected host content")
	}
	if releases != 1 {
		t.Fatalf("restart repeated an already completed backend detach: %d", releases)
	}
	if content, err := os.ReadFile(unexpected); err != nil || string(content) != "synthetic host contents" {
		t.Fatalf("partial detach deleted unrelated host data: %q, %v", content, err)
	}
	if err := os.Remove(unexpected); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Detach(context.Background(), id); err != nil || len(restarted.List()) != 0 {
		t.Fatalf("cleanup did not resume after the explicit conflict was removed: %v", err)
	}
}
