//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPTYCommandPreservesDimensionsAndExitCode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	err := runPTYCommand(ctx, []string{"--height", "33", "--width", "101", "--", "sh", "-c", "stty size; exit 37"}, strings.NewReader(""), &output)
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 37 || !bytes.Contains(output.Bytes(), []byte("33 101")) {
		t.Fatalf("terminal output = %q, error=%v", output.String(), err)
	}
}
