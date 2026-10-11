package docker

import (
	"context"
	"path/filepath"
	"testing"
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
