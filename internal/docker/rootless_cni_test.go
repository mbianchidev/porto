//go:build !windows

package docker

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestRootlessKitScriptsEnterRootlessKitNamespaces(t *testing.T) {
	t.Setenv("CONTAINERD_NAMESPACE", "")
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
	run := func(script string, args ...string) string {
		t.Helper()
		if len(args) == 0 {
			args = []string{"default", "cni-connect", "--network", "backend"}
		}
		command := exec.Command("sh", append([]string{"-c", script, "porto-runtime-helper"}, args...)...)
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
	if got := run(limaRuntimeHelperScript, "default", "cni-identities", "backend"); got != "helper ns=default cni-identities backend" {
		t.Fatalf("CNI identity reads entered RootlessKit: %q", got)
	}
}

func TestTaskInputWriterUsesTheHostPathOfRootlessFIFOs(t *testing.T) {
	runtimeDir := t.TempDir()
	hostFIFO := filepath.Join(runtimeDir, "containerd", "fifo", "1", "demo-stdin")
	if err := os.MkdirAll(filepath.Dir(hostFIFO), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(hostFIFO, 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	go func() {
		data, _ := os.ReadFile(hostFIFO)
		received <- string(data)
	}()
	command := exec.Command("sh", "-c", taskInputWriterScript, "porto-task-input", "/run/containerd/fifo/1/demo-stdin")
	command.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+runtimeDir)
	command.Stdin = strings.NewReader("rootless input")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("task input writer failed: %v: %s", err, output)
	}
	if got := <-received; got != "rootless input" {
		t.Fatalf("host FIFO received %q", got)
	}
}

func TestOnlyCNIMutationsEnterRootlessKit(t *testing.T) {
	runtime := &grpcContainerRuntime{lima: "porto-engine", namespace: "default"}
	for command, want := range map[string]string{
		"cni-connect": limaRootlessCNIHelperScript, "cni-disconnect": limaRootlessCNIHelperScript, "cni-check": limaRootlessCNIHelperScript,
		"cni-identities": limaRuntimeHelperScript, "task-metrics": limaRuntimeHelperScript,
	} {
		helper, err := runtime.runtimeHelperCommand(command)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(helper.Args, want) {
			t.Fatalf("%s helper args = %q, want script %q", command, helper.Args, want)
		}
	}
}
