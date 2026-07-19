//go:build !windows

package process

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCommandConfiguresProcessGroup(t *testing.T) {
	cmd, stdout, stderr, err := Command(context.Background(), t.TempDir(), "/bin/sh", "-c", "true")
	if err != nil {
		t.Fatalf("create command: %v", err)
	}
	defer stdout.Close()
	defer stderr.Close()

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("command must run in its own process group")
	}
}

func TestStreamReturnsCallbackError(t *testing.T) {
	want := errors.New("store failed")
	err := Stream(strings.NewReader("first\nsecond\n"), func(line string) error {
		if line == "second" {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("Stream error = %v, want %v", err, want)
	}
}
