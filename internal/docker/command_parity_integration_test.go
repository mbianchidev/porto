package docker

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
	dockerclient "github.com/moby/moby/client"
)

type parityIntegrationRunner struct {
	storageIntegrationRunner
}

func (runner parityIntegrationRunner) Start(ctx context.Context, command runtimes.Command) (runtimes.Process, error) {
	for index, argument := range command.Args {
		if argument == `exec "$HOME/.local/bin/porto-runtime-helper" pty-command "$@"` {
			command.Args = append([]string(nil), command.Args...)
			command.Args[index] = `exec "` + runner.helper + `" pty-command "$@"`
			break
		}
	}
	return (runtimes.ExecRunner{}).Start(ctx, command)
}

func TestDockerCommandParityLiveIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_PARITY_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_PARITY_INTEGRATION=1 to exercise an already running, owned Lima engine")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	manager := New(nil)
	ownership := manager.EngineOwnershipStatus(ctx)
	if !ownership.Owned || !ownership.Verified || ownership.Mode != "lima" {
		t.Fatalf("refusing an unverified engine: %+v", ownership)
	}
	localHelper := os.Getenv("PORTO_DOCKER_PARITY_TEST_HELPER")
	if !filepath.IsAbs(localHelper) {
		t.Fatal("PORTO_DOCKER_PARITY_TEST_HELPER must point to this revision's cross-built Linux helper")
	}
	runner := runtimes.ExecRunner{}
	probeOutput, err := runner.Run(ctx, runtimes.Command{Name: "limactl", Args: []string{
		"shell", "--workdir=/", ownership.Instance, "--", "mktemp", "-d", "/tmp/porto-parity-helper-XXXXXXXX",
	}})
	if err != nil {
		t.Fatal(err)
	}
	directory := strings.TrimSpace(string(probeOutput))
	if !strings.HasPrefix(directory, "/tmp/porto-parity-helper-") || strings.ContainsAny(directory, "\r\n\"\\") {
		t.Fatal("could not prove the temporary helper directory")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if _, err := runner.Run(cleanup, runtimes.Command{Name: "limactl", Args: []string{
			"shell", "--workdir=/", ownership.Instance, "--", "rm", "-r", "--", directory,
		}}); err != nil {
			t.Errorf("remove exact fixture helper directory: %v", err)
		}
	})
	guestHelper := directory + "/helper"
	for _, args := range [][]string{
		{"copy", localHelper, ownership.Instance + ":" + guestHelper},
		{"shell", "--workdir=/", ownership.Instance, "--", "chmod", "700", guestHelper},
	} {
		if _, err := runner.Run(ctx, runtimes.Command{Name: "limactl", Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	manager.runner = parityIntegrationRunner{storageIntegrationRunner{helper: guestHelper}}
	host := startDockerTestAPI(t, manager)
	client := dockerSDKClient(t, host)
	suite := "porto-parity-" + newInventoryInstanceID()
	image := suite + ":latest"
	containers := []string{suite + "-base", suite + "-flags", suite + "-ports", suite + "-tty"}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, name := range containers {
			if err := manager.ContainerAction(cleanupContext, name, "remove-force"); err != nil && !errorsIsNotFound(err) {
				t.Errorf("remove synthetic container %s: %v", name, err)
			}
		}
		if err := manager.RemoveImage(cleanupContext, image, true); err != nil && !errorsIsNotFound(err) {
			t.Errorf("remove synthetic image: %v", err)
		}
	})
	build, err := client.ImageBuild(ctx, bytes.NewReader(syntheticClientBuildArchive(t)), dockerclient.ImageBuildOptions{
		Tags: []string{image}, NoCache: true, Remove: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	readClientProgress(t, build.Body)
	environment := process.WithEnvironment(os.Environ(), "DOCKER_HOST="+host, "DOCKER_CONTEXT=",
		"DOCKER_CONFIG="+t.TempDir(), "DOCKER_TLS_VERIFY=", "DOCKER_CERT_PATH=")
	run := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(ctx, "docker", args...)
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", strings.Join(args, " "), err, output)
		}
		return string(output)
	}
	base, flags, ports, tty := containers[0], containers[1], containers[2], containers[3]
	run("create", "--name", base, "--network", "none", image, "sh", "-c", "echo synthetic-history; touch /synthetic-change; sleep 120")
	run("start", base)
	if output := run("top", base); !strings.Contains(output, "sleep") {
		t.Fatalf("missing live processes: %s", output)
	}
	run("exec", base, "test", "-f", "/synthetic-change")
	if output := run("diff", base); !strings.Contains(output, "/synthetic-change") {
		t.Fatalf("missing filesystem changes: %s", output)
	}
	run("history", image)
	run("images", "--all")
	run("ps", "--all", "--size", "--last", "2")
	run("update", "--cpus", "1", "--memory", "128m", "--pids-limit", "64", "--restart", "always", base)
	run("create", "--name", flags, "--network", "none", "--cpus", "1", "--memory", "64m", "--pids-limit", "64",
		"--read-only", "--cap-add", "NET_ADMIN", "--cap-drop", "MKNOD", "--dns", "192.0.2.53", "--add-host", "synthetic:192.0.2.10",
		"--group-add", "1234", "--ulimit", "nofile=1024:2048", image, "sh", "-c", "test ! -w /; sleep 120")
	run("start", flags)
	for _, check := range []struct{ path, expected string }{
		{"/sys/fs/cgroup/memory.max", "67108864"},
		{"/sys/fs/cgroup/pids.max", "64"},
		{"/sys/fs/cgroup/cpu.max", "100000 100000"},
	} {
		if value := strings.TrimSpace(run("exec", flags, "cat", check.path)); value != check.expected {
			t.Fatalf("actual cgroup limit %s = %q, want %q", check.path, value, check.expected)
		}
	}
	writeRoot := exec.CommandContext(ctx, "docker", "exec", flags, "touch", "/synthetic-must-not-write")
	writeRoot.Env = environment
	if output, err := writeRoot.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("Read-only file system")) {
		t.Fatalf("read-only root was not enforced: %v: %s", err, output)
	}
	run("create", "--name", ports, "--expose", "80", "-P", image, "sleep", "120")
	run("start", ports)
	if output := run("inspect", "--format", `{{(index (index .NetworkSettings.Ports "80/tcp") 0).HostPort}}`, ports); strings.TrimSpace(output) == "" {
		t.Fatal("publish-all did not allocate a port")
	}
	run("create", "--name", tty, "--network", "none", "--tty", "--interactive", image, "sh", "-c", "stty size; read unused; stty size; exit 37")
	if err := manager.configureContainerConsole(ctx, tty, [2]uint{33, 101}); err != nil {
		t.Fatal(err)
	}
	attached, err := client.ContainerAttach(ctx, tty, dockerclient.ContainerAttachOptions{Stream: true, Stdin: true, Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	defer attached.Close()
	if _, err := client.ContainerStart(ctx, tty, dockerclient.ContainerStartOptions{}); err != nil {
		_ = attached.Conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		diagnostics, _ := io.ReadAll(attached.Reader)
		t.Fatalf("start attached terminal: %v; stream=%q", err, diagnostics)
	}
	line := make([]byte, 64)
	count, err := attached.Reader.Read(line)
	if err != nil || !strings.Contains(string(line[:count]), "33 101") {
		t.Fatalf("initial terminal size = %q: %v", line[:count], err)
	}
	if _, err := client.ContainerResize(ctx, tty, dockerclient.ContainerResizeOptions{Height: 40, Width: 120}); err != nil {
		t.Fatal(err)
	}
	if _, err := attached.Conn.Write([]byte("\n")); err != nil {
		t.Fatal(err)
	}
	resized, err := io.ReadAll(attached.Reader)
	if err != nil || !bytes.Contains(resized, []byte("40 120")) {
		t.Fatalf("resized terminal output = %q: %v", resized, err)
	}
	if code := strings.TrimSpace(run("wait", tty)); code != "37" {
		t.Fatalf("terminal exit code = %q, want 37", code)
	}
	history, err := client.ContainerAttach(ctx, base, dockerclient.ContainerAttachOptions{Logs: true, Stdout: true, Stderr: true})
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(history.Reader)
	history.Close()
	if err != nil || !bytes.Contains(output, []byte("synthetic-history")) {
		t.Fatalf("historical attach = %q: %v", output, err)
	}
	t.Logf("real context-compatible routes and flags succeeded against %s", ownership.Instance)
}
