//go:build !windows

package process

import (
	"context"
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
