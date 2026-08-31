package main

import (
	"flag"
	"reflect"
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

func TestK9sTerminalArgsScopesClusterAndNamespace(t *testing.T) {
	got := k9sTerminalArgs("dev", "/tmp/dev.yaml", k9sTerminalOptions{
		Namespace: "platform",
		Command:   "deployments",
		ReadOnly:  true,
	})
	want := []string{
		"--kubeconfig", "/tmp/dev.yaml",
		"--context", "porto-dev",
		"--namespace", "platform",
		"--command", "deployments",
		"--readonly",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("k9s args = %#v, want %#v", got, want)
	}
}
