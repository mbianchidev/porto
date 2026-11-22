package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestSynchronizedShareOutputSeparatesSSHConfigurationFromDiagnostics(t *testing.T) {
	const configuration = "Host lima-porto-engine\n  HostName 127.0.0.1\n  Port 12345\n"
	runner := &fakeRunner{ordered: map[string][]runtimes.OutputChunk{
		"limactl show-ssh --format=config porto-engine": {
			{Stream: "stderr", Data: []byte("time=\"synthetic\" level=info msg=\"Reading SSH configuration\"\n")},
			{Stream: "stdout", Data: []byte(configuration)},
		},
	}}
	manager := NewWithStateDir(runner, t.TempDir())
	output, err := manager.synchronizedShareOutput(context.Background(), "read synthetic SSH configuration",
		"show-ssh", "--format=config", "porto-engine")
	if err != nil || string(output) != configuration {
		t.Fatalf("SSH configuration was contaminated by diagnostics: %q, %v", output, err)
	}
}

func TestSynchronizedShareKnownHostsMatchesLimaHostKeyAlias(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("OpenSSH ssh-keygen is unavailable")
	}
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQ"
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(synchronizedShareKnownHosts("lima-porto-engine", 12345, key)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"[127.0.0.1]:12345", "[::1]:12345", "lima-porto-engine"} {
		output, err := exec.Command(keygen, "-F", host, "-f", knownHosts).CombinedOutput()
		if err != nil || !strings.Contains(string(output), key) {
			t.Errorf("OpenSSH cannot find the pinned guest key for %s: %v: %s", host, err, output)
		}
	}
}

func TestSynchronizedShareOutputFailsWithoutReturningPartialConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		chunks []runtimes.OutputChunk
		err    error
	}{
		{name: "command failure", chunks: []runtimes.OutputChunk{{Stream: "stdout", Data: []byte("Host partial\n")}}, err: errors.New("synthetic failure")},
		{name: "oversized configuration", chunks: []runtimes.OutputChunk{{Stream: "stdout", Data: []byte(strings.Repeat("x", (64<<10)+1))}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{
				ordered: map[string][]runtimes.OutputChunk{"limactl show-ssh": test.chunks},
				errors:  map[string]error{"limactl show-ssh": test.err},
			}
			manager := NewWithStateDir(runner, t.TempDir())
			if output, err := manager.synchronizedShareOutput(context.Background(), "synthetic SSH read", "show-ssh"); err == nil || output != nil {
				t.Fatalf("invalid configuration was returned as usable: %q, %v", output, err)
			}
		})
	}
}
