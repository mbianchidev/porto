package runtimes

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalOwnershipCommandsDoNotDependOnProxyConfiguration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	if err := os.WriteFile(filepath.Join(root, "networking.json"), []byte("synthetic invalid proxy configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := (ExecRunner{}).Run(context.Background(), Command{
		Name: os.Args[0], Args: []string{"-test.run=^TestCommandInputHelper$"},
		Env: []string{"PORTO_COMMAND_INPUT_HELPER=1"}, Stdin: []byte("synthetic ownership probe"),
	})
	if err != nil || string(output) != "synthetic ownership probe" {
		t.Fatalf("local ownership/cleanup depended on proxy configuration: %q, %v", output, err)
	}
}
