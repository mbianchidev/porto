package runtimes

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
)

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
