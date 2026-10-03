//go:build windows

package nativefiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func TestNativeFixtureServerProcess(t *testing.T) {
	directory := os.Getenv("PORTO_NATIVE_SYNTHETIC_SFTP")
	if directory == "" {
		t.Skip("only runs as an isolated synthetic SFTP helper")
	}
	if err := datafiles.ServeSFTP(context.Background(), &nativeFixtureStdio{}, directory, datafiles.SFTPOptions{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type nativeFixtureStdio struct{}

func (*nativeFixtureStdio) Read(buffer []byte) (int, error)  { return os.Stdin.Read(buffer) }
func (*nativeFixtureStdio) Write(buffer []byte) (int, error) { return os.Stdout.Write(buffer) }
func (*nativeFixtureStdio) Close() error                     { return os.Stdin.Close() }

func TestWindowsNativeFoldersExposeDirectEditsReadonlyAndOwnedDetach(t *testing.T) {
	if os.Getenv("PORTO_TEST_WINFSP_NATIVE") != "1" {
		t.Skip("requires the explicitly provisioned WinFsp CI driver")
	}
	helper := os.Getenv("PORTO_TEST_NATIVE_MOUNT_HELPER")
	if helper == "" {
		t.Fatal("PORTO_TEST_NATIVE_MOUNT_HELPER must point at the installed package helper")
	}
	for _, kind := range []string{"container", "image", "volume", "vm"} {
		t.Run(kind, func(t *testing.T) {
			source := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "fixture.txt"), []byte("synthetic-original"), 0o600); err != nil {
				t.Fatal(err)
			}
			parent := filepath.Join(t.TempDir(), "Porto paths with spaces-\u03b4")
			resource := datafiles.Resource{Kind: kind, Name: "synthetic-fixture", ID: "fixture-" + kind, ReadOnly: kind == "image"}
			target := Target{
				Resource: resource, Identity: resource.Fingerprint(),
				SFTP: &SFTPCommand{
					Name: os.Args[0], Args: []string{"-test.run=^TestNativeFixtureServerProcess$"},
					Env: []string{"PORTO_NATIVE_SYNTHETIC_SFTP=" + source},
				},
				Verify:  func(context.Context) error { return nil },
				Release: func(context.Context) error { return nil },
			}
			manager := New(parent, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			attachment, err := manager.Attach(ctx, target, kind != "image")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				closeContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := manager.Close(closeContext); err != nil {
					t.Errorf("cleanup native fixture: %v", err)
				}
			})
			current, err := os.ReadFile(filepath.Join(attachment.Path, "fixture.txt"))
			if err != nil || string(current) != "synthetic-original" {
				t.Fatalf("real host path unreadable: %q %v", current, err)
			}
			if kind == "image" {
				if err := os.WriteFile(filepath.Join(attachment.Path, "fixture.txt"), []byte("forbidden"), 0o600); err == nil {
					t.Fatal("native image writes were not rejected")
				}
				if _, err := manager.Attach(ctx, target, true); !errors.Is(err, datafiles.ErrUnsupported) {
					t.Fatalf("explicit immutable write mount accepted: %v", err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(attachment.Path, "editor.tmp"), []byte("editor-write"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(filepath.Join(attachment.Path, "editor.tmp"), filepath.Join(attachment.Path, "saved.txt")); err != nil {
					t.Fatal(err)
				}
				current, err = os.ReadFile(filepath.Join(source, "saved.txt"))
				if err != nil || string(current) != "editor-write" {
					t.Fatalf("editor save did not directly reach workload files: %q %v", current, err)
				}
				if err := os.WriteFile(filepath.Join(source, "guest-new.txt"), []byte("guest-write"), 0o600); err != nil {
					t.Fatal(err)
				}
				current, err = os.ReadFile(filepath.Join(attachment.Path, "guest-new.txt"))
				if err != nil || string(current) != "guest-write" {
					t.Fatalf("concurrent guest change hidden by native cache: %q %v", current, err)
				}
			}
			if err := manager.Detach(ctx, attachment.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(attachment.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("owned native path survived detach: %v", err)
			}
			current, err = os.ReadFile(filepath.Join(source, "fixture.txt"))
			if err != nil || string(current) != "synthetic-original" {
				t.Fatalf("native cleanup changed source data: %q %v", current, err)
			}
		})
	}
}
