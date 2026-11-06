package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/nativefiles"
)

func runMountCommand(t *testing.T, input string, args ...string) (string, string, int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestMountCommandProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "PORTO_FILES_MOUNT_TEST_HELPER=1")
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("mount command did not exit: %v", ctx.Err())
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return stdout.String(), stderr.String(), exited.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), 0
}

func TestMountProbeReturnsActionableJSON(t *testing.T) {
	stdout, stderr, code := runMountCommand(t, "", "--probe")
	var capability nativefiles.Capability
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &capability) != nil {
		t.Fatalf("probe result = %d, %q, %q", code, stdout, stderr)
	}
	if capability.Driver == "" || capability.Message == "" {
		t.Fatalf("probe omitted its driver or capability explanation: %q", stdout)
	}
}

func TestMountCommandRejectsArgumentsBeforeReadingRequests(t *testing.T) {
	for _, args := range [][]string{{"--unknown"}, {"--probe", "unexpected"}} {
		stdout, stderr, code := runMountCommand(t, "", args...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "usage: porto-files-mount") {
			t.Fatalf("invalid arguments %q returned %d, %q, %q", args, code, stdout, stderr)
		}
	}
}

func TestMountCommandReportsInvalidOwnedInput(t *testing.T) {
	stdout, stderr, code := runMountCommand(t, "{invalid\n")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "Porto native mount:") {
		t.Fatalf("invalid mount request returned %d, %q, %q", code, stdout, stderr)
	}
}

func TestMountCommandProcess(t *testing.T) {
	if os.Getenv("PORTO_FILES_MOUNT_TEST_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"porto-files-mount"}, flag.Args()...)
	main()
	os.Exit(0)
}
