package acceleration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestGuestProbeVerifiesHandlersWithoutExternalParsers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the binfmt probe executes inside the Linux guest")
	}
	root := filepath.Join(t.TempDir(), "synthetic binfmt;$")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	interpreter := filepath.Join(root, "synthetic interpreter;$")
	notExecutable := filepath.Join(root, "synthetic nonexecutable")
	for path, mode := range map[string]os.FileMode{interpreter: 0o700, notExecutable: 0o600} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 99\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, contents := range map[string]string{
		"status":       "enabled\n",
		"qemu-x86_64":  "enabled\ninterpreter " + interpreter + "\nflags: OCF\n",
		"rosetta":      "disabled\ninterpreter " + interpreter + "\nflags: F\n",
		"qemu-aarch64": "enabled\nflags: F\n",
		"qemu-arm":     "disabled\ninterpreter " + interpreter + "\nflags: F\n",
		"qemu-i386":    "enabled\ninterpreter " + interpreter + "\nflags: OC\n",
		"qemu-riscv64": "enabled\ninterpreter " + filepath.Join(root, "missing") + "\nflags: F\n",
		"qemu-ppc64le": "enabled\ninterpreter " + notExecutable + "\nflags: F\n",
		"qemu-s390x":   "enabled\ninterpreter " + filepath.Join(root, "missing") + "\ninterpreter " + interpreter + "\nflags: F\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	const assignment = "root=/proc/sys/fs/binfmt_misc"
	if !strings.Contains(GuestProbeScript, assignment) {
		t.Fatal("the synthetic probe must redirect binfmt reads away from the host")
	}
	script := "uname() { printf '%s\\n' aarch64; }\n" + strings.Replace(GuestProbeScript, assignment,
		"root='"+strings.ReplaceAll(root, "'", "'\\''")+"'", 1)
	probe := func() Capabilities {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, "/bin/sh", "-c", script)
		command.Env = []string{"PATH=" + bin}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("read live synthetic binfmt state: %v: %s", err, output)
		}
		capabilities, err := WithGuestProbe(Capabilities{}, output)
		if err != nil {
			t.Fatal(err)
		}
		return capabilities
	}
	capabilities := probe()
	if capabilities.GuestArchitecture != "arm64" || !capabilities.Binfmt.Supported ||
		capabilities.AMD64Handler != "qemu" || len(capabilities.Handlers) != 1 {
		t.Fatalf("valid handler was lost or disabled/unsafe handlers were advertised: %+v", capabilities)
	}
	selection, err := Select(capabilities, Automatic, "linux/amd64")
	if err != nil || selection.Execution != "qemu" {
		t.Fatalf("the verified translator was not usable for a foreign build: %+v, %v", selection, err)
	}
	if err := os.WriteFile(filepath.Join(root, "status"), []byte("disabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capabilities = probe()
	if capabilities.GuestArchitecture != "arm64" || capabilities.Binfmt.Supported || len(capabilities.Handlers) != 0 {
		t.Fatalf("disabled binfmt retained stale translator capabilities: %+v", capabilities)
	}
	if _, err := Select(capabilities, Automatic, "linux/amd64"); err == nil {
		t.Fatal("a foreign build was accepted after its translator was disabled")
	}
}
