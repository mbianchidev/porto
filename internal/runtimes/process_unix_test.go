//go:build !windows

package runtimes

import (
	"bufio"
	"context"
	"io"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
)

func TestRuntimeProcessKillReleasesOwnedDescendantPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	child, err := (ExecRunner{}).Start(ctx, Command{
		Name: "/bin/sh", Args: []string{"-c", "sleep 60 & printf 'ready\\n'; wait"},
	})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := child.(*execProcess)
	if !ok {
		t.Fatal("expected a native runtime process")
	}
	t.Cleanup(func() {
		if !native.exited.Load() {
			_ = process.Kill(native.command)
		}
		_ = child.Wait()
	})
	reader := bufio.NewReader(child.Stdout())
	if ready, err := reader.ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatalf("owned descendant did not become ready: %q, %v", ready, err)
	}
	closed := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); closed <- err }()
	if err := child.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("owned descendant output failed to close cleanly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("killing the runtime parent left an owned descendant holding its output pipe and SSH session")
	}
	_ = child.Wait()
}
