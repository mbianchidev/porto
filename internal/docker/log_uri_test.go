package docker

import (
	"context"
	"testing"
)

func TestBinaryLoggerURIContainsActualCLIFlag(t *testing.T) {
	runtime := &grpcContainerRuntime{helperPath: "/synthetic/porto-runtime-helper"}
	uri, err := runtime.directLogURI(context.Background(), "/synthetic/container.log")
	if err != nil {
		t.Fatal(err)
	}
	if uri.Scheme != "binary" || uri.Query().Get("-porto-log") != "/synthetic/container.log" ||
		uri.Query().Has("porto-log") {
		t.Fatalf("containerd passes query keys verbatim as argv; logger URI = %s", uri)
	}
}
