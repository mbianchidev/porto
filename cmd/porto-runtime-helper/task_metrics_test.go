package main

import (
	"context"
	"io"
	"testing"
)

func TestTaskMetricsRefusesInvalidBackendRequests(t *testing.T) {
	for _, arguments := range [][]string{
		nil,
		{"--address", "relative", "--namespace", "synthetic", "--id", "synthetic"},
		{"--address", "/tmp/synthetic.sock", "--namespace", "", "--id", "synthetic"},
	} {
		if err := writeTaskMetrics(context.Background(), arguments, io.Discard); err == nil {
			t.Fatalf("invalid metric request was accepted: %v", arguments)
		}
	}
}
