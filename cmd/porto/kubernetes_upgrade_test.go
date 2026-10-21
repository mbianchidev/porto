package main

import (
	"strings"
	"testing"
)

func TestKubernetesUpgradeCLIRequiresVersionAndConfirmation(t *testing.T) {
	for _, args := range [][]string{
		{"upgrade", "fixture", "--version", "v1.36.1+k3s1"},
		{"upgrade", "fixture", "--confirm"},
		{"upgrade-recover", "fixture"},
		{"upgrade", "--version", "v1.36.1+k3s1", "--confirm"},
	} {
		if err := kubernetesClusterCmd(args); err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("arguments %v returned %v", args, err)
		}
	}
}
