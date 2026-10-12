package docker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestBinaryLoggerURIContainsActualCLIFlag(t *testing.T) {
	directory := t.TempDir()
	logPath := filepath.Join(directory, "container.log")
	runtime := &grpcContainerRuntime{helperPath: filepath.Join(directory, "porto-runtime-helper")}
	uri, err := runtime.directLogURI(context.Background(), logPath)
	if err != nil {
		t.Fatal(err)
	}

	if uri.Scheme != "binary" || uri.Query().Get("-porto-log") != logPath ||
		uri.Query().Has("porto-log") {
		t.Fatalf("containerd passes query keys verbatim as argv; logger URI = %s", uri)
	}
}

func TestLimaLogPathsRemainPOSIXOnEveryDesktopHost(t *testing.T) {
	runtime := &grpcContainerRuntime{lima: "synthetic-owned"}
	path, err := runtime.directContainerLogPath("synthetic")
	if err != nil || path != "/tmp/porto-container-logs/synthetic.log" {
		t.Fatalf("Linux backend log path = %q, error=%v", path, err)
	}
}

func TestLimaLoggerUsesInstalledGuestHelperWithoutHostExecutableSuffix(t *testing.T) {
	runtime := &grpcContainerRuntime{lima: "synthetic-owned", namespace: "synthetic",
		runner: &fakeRunner{handler: func(command runtimes.Command) ([]byte, error) {
			if command.Name != "limactl" || command.Args[len(command.Args)-1] != "path" {
				t.Fatalf("guest helper path was not resolved: %+v", command)
			}
			return []byte("/home/synthetic/.local/bin/porto-runtime-helper\n"), nil
		}}}
	uri, err := runtime.directLogURI(context.Background(), "/tmp/synthetic.log")
	if err != nil || uri.Scheme != "binary" || uri.Path != "/home/synthetic/.local/bin/porto-runtime-helper" ||
		uri.Query().Get("-porto-log") != "/tmp/synthetic.log" {
		t.Fatalf("guest-native logger URI = %v, error=%v", uri, err)
	}
}
