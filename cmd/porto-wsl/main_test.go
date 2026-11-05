package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/wslbridge"
)

func runWSLCommand(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestWSLCommandProcess$", "--"}, args...)...)
	command.Env = append(os.Environ(), "PORTO_WSL_TEST_HELPER=1")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("WSL command did not exit: %v", ctx.Err())
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

func TestWSLCommandReadOnlyActionsAndErrors(t *testing.T) {
	directory := t.TempDir()
	configuration := wslbridge.Config{
		Distribution:      "synthetic distribution",
		Owner:             "synthetic-owner",
		WindowsExecutable: filepath.Join(directory, "synthetic-porto.exe"),
		WindowsHome:       filepath.Join(directory, "synthetic-home"),
		Socket:            filepath.Join(directory, "synthetic.sock"),
	}
	data, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "configuration with spaces.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(directory, "invalid.json")
	if err := os.WriteFile(invalid, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		args   []string
		output string
		error  string
	}{
		{name: "version without configuration", args: []string{"version"}, output: config.Version + "\n"},
		{name: "owned endpoint", args: []string{"endpoint", path}, output: wslbridge.Endpoint(configuration) + "\n"},
		{name: "missing action", error: "usage: porto-wsl"},
		{name: "unknown action", args: []string{"unknown", path}, error: `unknown WSL helper action "unknown"`},
		{name: "missing credential action", args: []string{"credential", path}, error: "credential action is required"},
		{name: "invalid configuration", args: []string{"endpoint", invalid}, error: "invalid owned WSL bridge configuration"},
		{name: "missing configuration", args: []string{"endpoint", filepath.Join(directory, "missing.json")}, error: "missing.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := runWSLCommand(t, test.args...)
			if test.error != "" {
				if code != 1 || stdout != "" || !strings.Contains(stderr, "porto-wsl:") || !strings.Contains(stderr, test.error) {
					t.Fatalf("command error = %d, %q, %q", code, stdout, stderr)
				}
			} else if code != 0 || stdout != test.output || stderr != "" {
				t.Fatalf("command result = %d, %q, %q", code, stdout, stderr)
			}
		})
	}
}

func TestWSLCommandProcess(t *testing.T) {
	if os.Getenv("PORTO_WSL_TEST_HELPER") != "1" {
		return
	}
	os.Args = append([]string{"porto-wsl"}, flag.Args()...)
	main()
	os.Exit(0)
}
