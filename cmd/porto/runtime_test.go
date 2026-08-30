package main

import (
	"flag"
	"testing"
)

func TestParseInterspersedAllowsFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	count := fs.Int("count", 0, "")
	force := fs.Bool("force", false, "")
	if err := parseInterspersed(fs, []string{"cluster", "workers", "--count", "3", "--force"}, map[string]bool{"force": true}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *count != 3 || !*force {
		t.Fatalf("flags not parsed: count=%d force=%v", *count, *force)
	}
	if fs.NArg() != 2 || fs.Arg(0) != "cluster" || fs.Arg(1) != "workers" {
		t.Fatalf("positionals = %v", fs.Args())
	}
}
