package daemon

import "testing"

func TestAllowedShell(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "ash", "/bin/sh", "/bin/bash", "/bin/ash"} {
		if !allowedShell(shell) {
			t.Errorf("expected %q to be allowed", shell)
		}
	}
	for _, shell := range []string{"zsh", "sh -c id", "../../bin/sh", ""} {
		if allowedShell(shell) {
			t.Errorf("expected %q to be rejected", shell)
		}
	}
}
