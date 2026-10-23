package nativefiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func TestSSHFSUnmountDoesNotHideOtherStreamErrors(t *testing.T) {
	unexpected := errors.New("synthetic transfer failure")
	for _, scenario := range []struct {
		err    error
		normal bool
	}{
		{nil, true},
		{os.ErrClosed, true},
		{io.ErrClosedPipe, true},
		{&os.PathError{Op: "write", Path: "|1", Err: syscall.EPIPE}, true},
		{errors.Join(os.ErrClosed, syscall.EPIPE), true},
		{io.ErrUnexpectedEOF, false},
		{unexpected, false},
		{errors.Join(os.ErrClosed, unexpected), false},
		{fmt.Errorf("wrapped: %w", errors.Join(syscall.EPIPE, unexpected)), false},
	} {
		if got := sshfsClosedPipeErrors(scenario.err); got != scenario.normal {
			t.Errorf("unmount stream error classification = %v, want %v: %v", got, scenario.normal, scenario.err)
		}
	}
}

func TestSSHFSUnmountSignalIsNotAGeneralExitFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SSHFS uses Unix process signals")
	}
	for _, scenario := range []struct {
		command string
		normal  bool
	}{
		{"exit 0", false},
		{"exit 7", false},
		{"kill -TERM $$", true},
		{"kill -KILL $$", false},
	} {
		t.Run(scenario.command, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := exec.CommandContext(ctx, "sh", "-c", scenario.command).Run()
			if err != nil {
				err = fmt.Errorf("SSHFS bridge failed: %w", err)
			}
			if sshfsUnmountSignal(err) != scenario.normal {
				t.Fatalf("unexpected detach exit classification: %v", err)
			}
		})
	}
	if sshfsUnmountSignal(errors.New("unrelated bridge error")) || sshfsUnmountSignal(&exec.ExitError{}) {
		t.Fatal("an unknown bridge failure was accepted as normal unmount")
	}
}
