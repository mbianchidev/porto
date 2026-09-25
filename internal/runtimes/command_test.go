package runtimes

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/logging"
)

func TestRuntimeDebugLoggingDoesNotExposeCommandPayloads(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "combined", true: "streaming"}[streaming], func(t *testing.T) {
			directory := t.TempDir()
			stderr, err := os.Create(filepath.Join(directory, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			logPath := filepath.Join(directory, "porto.log")
			closeLog, err := logging.Open(logPath, "", stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer closeLog()
			command := Command{
				Name:  os.Args[0],
				Args:  []string{"-test.run=^TestCommandInputHelper$", "--", "synthetic-private-argument"},
				Env:   []string{"PORTO_COMMAND_INPUT_HELPER=1", "SYNTHETIC_PRIVATE_ENV=synthetic-private-environment"},
				Stdin: []byte("synthetic-private-input"),
			}
			if streaming {
				_, err = (ExecRunner{}).RunStreaming(context.Background(), command, func(OutputChunk) error { return nil })
			} else {
				_, err = (ExecRunner{}).Run(context.Background(), command)
			}
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"level=DEBUG", "Runtime command started", "Runtime command finished", "duration="} {
				if !strings.Contains(string(contents), expected) {
					t.Errorf("runtime diagnostics are missing %q: %s", expected, contents)
				}
			}
			if strings.Contains(string(contents), "synthetic-private") {
				t.Fatalf("runtime diagnostics exposed arguments, environment, or stream data: %s", contents)
			}
		})
	}
}

func TestExecRunnerReadsInputFromStream(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := (ExecRunner{}).Run(context.Background(), Command{
		Name:        executable,
		Args:        []string{"-test.run=TestCommandInputHelper"},
		Env:         []string{"PORTO_COMMAND_INPUT_HELPER=1"},
		StdinReader: bytes.NewReader([]byte("build context")),
	})
	if err != nil {
		t.Fatalf("run helper: %v: %s", err, output)
	}
	if string(output) != "build context" {
		t.Fatalf("output = %q", output)
	}
}

func TestCommandInputHelper(t *testing.T) {
	if os.Getenv("PORTO_COMMAND_INPUT_HELPER") != "1" {
		return
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
