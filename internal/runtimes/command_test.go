package runtimes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/logging"
)

func TestRuntimeDebugLoggingDoesNotExposeCommandPayloads(t *testing.T) {
	for _, mode := range []string{"combined", "streaming", "stdout-only"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			stderr, err := os.Create(filepath.Join(directory, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			logPath := filepath.Join(directory, "porto.log")
			diagnostics, err := logging.Open(logPath, "", stderr)
			if err != nil {
				t.Fatal(err)
			}
			defer diagnostics.Close()
			command := Command{
				Name:  os.Args[0],
				Args:  []string{"-test.run=^TestCommandInputHelper$", "--", "synthetic-private-argument"},
				Env:   []string{"PORTO_COMMAND_INPUT_HELPER=1", "SYNTHETIC_PRIVATE_ENV=synthetic-private-environment"},
				Stdin: []byte("synthetic-private-input"),
			}
			if mode == "stdout-only" {
				command.StdoutOnly = true
				command.Env = append(command.Env, "PORTO_COMMAND_INPUT_STDERR=1")
			}
			if mode == "streaming" {
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

func TestStructuredCommandOutputSeparatesDiagnosticsAndFailsWithoutPartialData(t *testing.T) {
	for _, mode := range []string{"success", "failure", "large-diagnostic"} {
		t.Run(mode, func(t *testing.T) {
			output, err := (ExecRunner{}).Run(context.Background(), Command{
				Name: os.Args[0], Args: []string{"-test.run=^TestStructuredCommandHelper$"},
				Env: []string{"PORTO_STRUCTURED_COMMAND_HELPER=" + mode}, StdoutOnly: true,
			})
			if mode == "failure" {
				var exited interface{ ExitCode() int }
				if !errors.As(err, &exited) || exited.ExitCode() != 19 || string(output) != "mesg: synthetic diagnostic\n" {
					t.Fatalf("failed query returned partial state or lost diagnostics: %.512q, %v", output, err)
				}
			} else if err != nil || string(output) != `[{"Id":"synthetic-container"}]` {
				t.Fatalf("structured query was contaminated by diagnostics: %.512q, %v", output, err)
			}
		})
	}
}

func TestStructuredCommandHelper(t *testing.T) {
	mode := os.Getenv("PORTO_STRUCTURED_COMMAND_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprint(os.Stdout, `[{"Id":"synthetic-container"}]`)
	fmt.Fprintln(os.Stderr, "mesg: synthetic diagnostic")
	if mode == "large-diagnostic" {
		fmt.Fprint(os.Stderr, strings.Repeat("synthetic diagnostic\n", 8192))
	}
	if mode == "failure" {
		os.Exit(19)
	}
	os.Exit(0)
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
	if os.Getenv("PORTO_COMMAND_INPUT_STDERR") == "1" {
		fmt.Fprintln(os.Stderr, "synthetic-private-stderr")
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
