//go:build !windows

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootlessCNIHelperEntersRootlessKitNamespaces(t *testing.T) {
	directory := t.TempDir()
	home, bin, runtimeDir := filepath.Join(directory, "home"), filepath.Join(directory, "bin"), filepath.Join(directory, "run")
	helper := filepath.Join(home, ".local", "bin", "porto-runtime-helper")
	for _, script := range []struct{ path, body string }{
		{helper, "#!/bin/sh\necho \"helper ns=$CONTAINERD_NAMESPACE $*\"\n"},
		{filepath.Join(bin, "nsenter"), "#!/bin/sh\necho \"nsenter ns=$CONTAINERD_NAMESPACE $*\"\n"},
	} {
		if err := os.MkdirAll(filepath.Dir(script.path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(script.path, []byte(script.body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(script string) string {
		t.Helper()
		command := exec.Command("sh", "-c", script, "porto-runtime-helper", "default", "cni-connect", "--network", "backend")
		command.Env = append(os.Environ(), "HOME="+home, "XDG_RUNTIME_DIR="+runtimeDir, "PATH="+bin+":/usr/bin:/bin")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("helper script failed: %v: %s", err, output)
		}
		return strings.TrimSpace(string(output))
	}

	if got := run(limaRootlessCNIHelperScript); got != "helper ns=default cni-connect --network backend" {
		t.Fatalf("rootful CNI helper = %q", got)
	}
	if err := os.MkdirAll(filepath.Join(runtimeDir, "containerd-rootless"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "containerd-rootless", "child_pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "nsenter ns=default -U --preserve-credentials -m -n -t 4242 -F -- " + helper + " cni-connect --network backend"
	if got := run(limaRootlessCNIHelperScript); got != want {
		t.Fatalf("rootless CNI helper = %q, want %q", got, want)
	}
	if got := run(limaRuntimeHelperScript); got != "helper ns=default cni-connect --network backend" {
		t.Fatalf("non-CNI helper entered RootlessKit: %q", got)
	}
}
