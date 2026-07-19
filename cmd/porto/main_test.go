package main

import (
	"testing"
)

func TestParseLogArgsAllowsOptionsBeforeAndAfterProject(t *testing.T) {
	for _, args := range [][]string{
		{"--stream", "stderr", "-n", "50", "app"},
		{"app", "--stream=stderr", "-n=50"},
	} {
		project, stream, limit, clear, err := parseLogArgs(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if project != "app" || stream != "stderr" || limit != 50 || clear {
			t.Fatalf("parse %v = %q, %q, %d, %t", args, project, stream, limit, clear)
		}
	}
}

func TestParseLogArgsClear(t *testing.T) {
	project, stream, _, clear, err := parseLogArgs([]string{"app", "--clear", "--stream", "stdout"})
	if err != nil {
		t.Fatalf("parse clear: %v", err)
	}
	if project != "app" || stream != "stdout" || !clear {
		t.Fatalf("clear args = %q, %q, %t", project, stream, clear)
	}
}
