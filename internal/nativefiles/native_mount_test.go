package nativefiles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/datafiles"
)

func TestNativeFixtureServerProcess(t *testing.T) {
	directory := os.Getenv("PORTO_NATIVE_SYNTHETIC_SFTP")
	if directory == "" {
		t.Skip("only runs as an isolated synthetic SFTP helper")
	}
	if err := datafiles.ServeSFTP(context.Background(), &nativeFixtureStdio{}, directory, datafiles.SFTPOptions{
		ReadOnly: os.Getenv("PORTO_NATIVE_SYNTHETIC_READONLY") == "1",
	}); err != nil {
		fmt.Fprintln(os.Stderr, "synthetic native filesystem server:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

type nativeFixtureStdio struct{}

func (*nativeFixtureStdio) Read(buffer []byte) (int, error)  { return os.Stdin.Read(buffer) }
func (*nativeFixtureStdio) Write(buffer []byte) (int, error) { return os.Stdout.Write(buffer) }
func (*nativeFixtureStdio) Close() error                     { return os.Stdin.Close() }

func syntheticNativeTarget(t *testing.T, kind string) (Target, string) {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "synthetic.txt"), []byte("synthetic-original"), 0o640); err != nil {
		t.Fatal(err)
	}
	resource := datafiles.Resource{Kind: kind, Name: "synthetic-fixture", ID: "fixture-" + kind, ReadOnly: kind == "image"}
	readOnly := "0"
	if resource.ReadOnly {
		readOnly = "1"
	}
	return Target{
		Resource: resource, Identity: resource.Fingerprint(),
		SFTP: &SFTPCommand{
			Name: os.Args[0], Args: []string{"-test.run=^TestNativeFixtureServerProcess$"},
			Env: []string{"PORTO_NATIVE_SYNTHETIC_SFTP=" + source, "PORTO_NATIVE_SYNTHETIC_READONLY=" + readOnly},
		},
		Verify:  func(context.Context) error { return nil },
		Release: func(context.Context) error { return nil },
	}, source
}
