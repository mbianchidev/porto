package syncshares

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestMaintainedSynchronizationPreservesLargeSyntheticRepository(t *testing.T) {
	binary := os.Getenv("PORTO_TEST_SYNC_BINARY")
	if binary == "" {
		t.Skip("set PORTO_TEST_SYNC_BINARY to Porto's patched toolchain for real maintained-tool acceptance")
	}
	if runtime.GOOS == "windows" {
		t.Fatal("optimized sharing is explicitly unsupported on Windows hosts")
	}
	root, err := os.MkdirTemp("/tmp", "porto-sync-real-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	alpha, beta := filepath.Join(root, "alpha"), filepath.Join(root, "beta")
	for _, directory := range []string{alpha, beta, filepath.Join(alpha, "node_modules"), filepath.Join(alpha, ".git")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 10000 {
		directory := filepath.Join(alpha, fmt.Sprintf("package-%03d", index/100))
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("fixture-%05d.txt", index)), []byte(fmt.Sprintf("synthetic repository entry %d\n", index)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, ignored := range []string{"node_modules/ignored.txt", ".git/ignored.txt"} {
		if err := os.WriteFile(filepath.Join(alpha, ignored), []byte("synthetic ignored data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(alpha, "executable"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("executable", filepath.Join(alpha, "relative-link")); err != nil {
		t.Fatal(err)
	}
	provider := &syntheticProvider{owner: "synthetic-native-owner", beta: beta}
	manager := New(filepath.Join(root, "config"), nil, provider)
	manager.lookPath = func(name string) (string, error) {
		if name == "mutagen" {
			return binary, nil
		}
		return exec.LookPath(name)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := manager.Close(ctx); err != nil {
			t.Error(err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	started := time.Now()
	share, err := manager.Create(ctx, 1, alpha, []string{"node_modules"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("10,000-entry synthetic repository initial synchronization: %s on %s/%s", time.Since(started), runtime.GOOS, runtime.GOARCH)
	if _, err := os.Stat(filepath.Join(beta, "package-099", "fixture-09999.txt")); err != nil {
		t.Fatal(err)
	}
	for _, ignored := range []string{"node_modules/ignored.txt", ".git/ignored.txt"} {
		if _, err := os.Stat(filepath.Join(beta, ignored)); !os.IsNotExist(err) {
			t.Fatalf("ignored data was copied: %s, %v", ignored, err)
		}
	}
	if info, err := os.Stat(filepath.Join(beta, "executable")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("portable executable permission was lost: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(beta, "relative-link")); err != nil || target != "executable" {
		t.Fatalf("relative contained symlink changed: %q, %v", target, err)
	}
	if err := os.WriteFile(filepath.Join(alpha, "host-notification.txt"), []byte("synthetic host edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitSyncFile(t, filepath.Join(beta, "host-notification.txt"), "synthetic host edit")
	if err := os.WriteFile(filepath.Join(beta, "guest-notification.txt"), []byte("synthetic guest edit"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitSyncFile(t, filepath.Join(alpha, "guest-notification.txt"), "synthetic guest edit")
	if err := os.Rename(filepath.Join(alpha, "host-notification.txt"), filepath.Join(alpha, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	waitSyncFile(t, filepath.Join(beta, "renamed.txt"), "synthetic host edit")
	if err := manager.SetMode(ctx, share.ID, "bind"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alpha, "conflict.txt"), []byte("synthetic host version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta, "conflict.txt"), []byte("synthetic guest version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetMode(ctx, share.ID, "synchronized"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.command(ctx, "sync", "flush", share.SessionID); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Shares) != 1 || status.Shares[0].Session == nil || len(status.Shares[0].Session.Conflicts) == 0 {
		t.Fatalf("concurrent edits were not reported as a conflict: %+v", status)
	}
	if _, err := manager.Resolve(ctx, alpha); err == nil {
		t.Fatal("conflicting data was exposed as a ready mount")
	}
	for _, side := range []struct{ path, expected string }{{alpha, "synthetic host version"}, {beta, "synthetic guest version"}} {
		data, err := os.ReadFile(filepath.Join(side.path, "conflict.txt"))
		if err != nil || string(data) != side.expected {
			t.Fatalf("a conflict version was discarded: %s, %v", data, err)
		}
	}
}

func waitSyncFile(t *testing.T, path, expected string) {
	t.Helper()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == expected {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("maintained filesystem notifications did not propagate %s: %v", filepath.Base(path), err)
		case <-ticker.C:
		}
	}
}
