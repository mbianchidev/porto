package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
)

func TestDockerRunAndCLIPreserveArgumentsEnvironmentAndExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "docker.txt")
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(
		"#!/bin/sh\nprintf '%s\\n' \"$DOCKER_HOST\" \"$DOCKER_CONTEXT\" \"$@\" > \"$DOCKER_CAPTURE\"\nexit \"$DOCKER_EXIT_CODE\"\n",
	), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv("DOCKER_CAPTURE", capture)
	t.Setenv("DOCKER_HOST", "unix:///synthetic-other.sock")
	t.Setenv("DOCKER_CONTEXT", "synthetic-other")
	endpoint, err := config.DockerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	runArgs := []string{"run", "--rm", "--env", "SYNTHETIC=value with spaces", "alpine:latest", "echo", "hello docker", "--literal"}
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "run",
			args: append([]string{"docker"}, runArgs...),
			want: append([]string{endpoint, "", "--host", endpoint}, runArgs...),
		},
		{
			name: "cli",
			args: append([]string{"docker", "cli", "--context", "porto"}, runArgs...),
			want: append([]string{"unix:///synthetic-other.sock", "synthetic-other", "--context", "porto"}, runArgs...),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, code := range []string{"0", "23", "125"} {
				t.Run(code, func(t *testing.T) {
					t.Setenv("DOCKER_EXIT_CODE", code)
					err := runCommand(test.args)
					if code == "0" {
						if err != nil {
							t.Fatalf("docker command: %v", err)
						}
					} else {
						var exitError *exec.ExitError
						if !errors.As(err, &exitError) || strconv.Itoa(exitError.ExitCode()) != code {
							t.Fatalf("docker exit error = %v, want exit code %s", err, code)
						}
					}
					data, err := os.ReadFile(capture)
					if err != nil {
						t.Fatal(err)
					}
					want := strings.Join(test.want, "\n") + "\n"
					if string(data) != want {
						t.Fatalf("docker invocation = %q, want %q", data, want)
					}
				})
			}
		})
	}
}

func TestDockerRunRequiresArguments(t *testing.T) {
	err := dockerCmd([]string{"run"})
	if err == nil || !strings.Contains(err.Error(), "usage: porto docker run") {
		t.Fatalf("docker run without arguments = %v", err)
	}
}

func TestDockerRunAndCLIPreserveStandardStreams(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"),
		[]byte("#!/bin/sh\ncat\nprintf 'synthetic stderr\\n' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_HOME", t.TempDir())
	for _, command := range []string{"run", "cli"} {
		t.Run(command, func(t *testing.T) {
			directory := t.TempDir()
			var streams [3]*os.File
			for index, name := range []string{"stdin", "stdout", "stderr"} {
				stream, err := os.Create(filepath.Join(directory, name))
				if err != nil {
					t.Fatal(err)
				}
				streams[index] = stream
				t.Cleanup(func() {
					if err := stream.Close(); err != nil {
						t.Errorf("close %s: %v", name, err)
					}
				})
			}
			if _, err := streams[0].WriteString("synthetic stdin\n"); err != nil {
				t.Fatal(err)
			}
			if _, err := streams[0].Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			stdin, stdout, stderr := os.Stdin, os.Stdout, os.Stderr
			t.Cleanup(func() { os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr })
			os.Stdin, os.Stdout, os.Stderr = streams[0], streams[1], streams[2]
			if err := dockerCmd([]string{command, "alpine:latest", "cat"}); err != nil {
				t.Fatalf("docker %s: %v", command, err)
			}
			for name, want := range map[string]string{"stdout": "synthetic stdin\n", "stderr": "synthetic stderr\n"} {
				data, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil || string(data) != want {
					t.Fatalf("%s = %q, error = %v; want %q", name, data, err, want)
				}
			}
		})
	}
}

func TestDockerPrivilegeHintPreservesRequestedOperation(t *testing.T) {
	t.Setenv("PORTO_HOME", filepath.Join(t.TempDir(), "Porto state"))
	activate := dockerPrivilegeHint(
		errors.New("remove previous Docker endpoint link: permission denied"),
		"activate",
		"--replace",
	)
	if !strings.Contains(activate.Error(), "docker activate --replace") ||
		strings.Contains(activate.Error(), "docker deactivate") {
		t.Fatalf("activate privilege hint = %q", activate)
	}
	deactivate := dockerPrivilegeHint(
		errors.New("remove Porto Docker endpoint: permission denied"),
		"deactivate",
	)
	if !strings.Contains(deactivate.Error(), "docker deactivate") {
		t.Fatalf("deactivate privilege hint = %q", deactivate)
	}
}

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

func TestDockerDiveTargetsPortoEndpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bin := t.TempDir()
	capture := filepath.Join(t.TempDir(), "dive.txt")
	dockerCapture := filepath.Join(t.TempDir(), "docker.txt")
	dive := filepath.Join(bin, "dive")
	if err := os.WriteFile(
		dive,
		[]byte("#!/bin/sh\nprintf '%s\\n%s\\n%s\\n' \"$DOCKER_HOST\" \"$DOCKER_CONTEXT\" \"$*\" > \"$DIVE_CAPTURE\"\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bin, "docker"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$DOCKER_CAPTURE\"\nprintf '%s\\n' 'node:22-alpine'\n"),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_HOME", t.TempDir())
	t.Setenv("DIVE_CAPTURE", capture)
	t.Setenv("DOCKER_CAPTURE", dockerCapture)
	t.Setenv("DOCKER_HOST", "unix:///wrong.sock")
	t.Setenv("DOCKER_CONTEXT", "wrong")

	if err := dockerCmd([]string{"dive", "alpine:latest", "--source", "docker"}); err != nil {
		t.Fatalf("docker dive: %v", err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "/run/docker.sock") ||
		lines[1] != "" || lines[2] != "alpine:latest --source docker" {
		t.Fatalf("unexpected Dive environment: %q", data)
	}
	if err := dockerCmd([]string{"dive", "--container", "demo", "--source", "docker"}); err != nil {
		t.Fatalf("docker dive container: %v", err)
	}
	data, err = os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(data)), "node:22-alpine --source docker") {
		t.Fatalf("container image was not forwarded to Dive: %q", data)
	}
	dockerArgs, err := os.ReadFile(dockerCapture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerArgs), "container inspect --format {{.Config.Image}} demo") {
		t.Fatalf("unexpected Docker inspect arguments: %q", dockerArgs)
	}
}

func TestK9sTerminalArgsScopesClusterAndNamespace(t *testing.T) {
	got := k9sTerminalArgs("porto-dev", "/tmp/dev.yaml", k9sTerminalOptions{
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

func TestK9sTerminalArgsUseK3sContextName(t *testing.T) {
	got := k9sTerminalArgs("porto-k3s-dev", "/tmp/dev.yaml", k9sTerminalOptions{})
	want := []string{
		"--kubeconfig", "/tmp/dev.yaml",
		"--context", "porto-k3s-dev",
		"--all-namespaces",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("k9s args = %#v, want %#v", got, want)
	}
}

func TestDefaultKubeconfigPathAlwaysUsesStandardLocation(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "custom"))
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	path, err := defaultKubeconfigPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".kube", "config")
	if path != want {
		t.Fatalf("default kubeconfig path = %q, want %q", path, want)
	}
}

func TestClusterKubeconfigOwnerReadsManagedMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PORTO_HOME", home)
	request := kubernetes.ClusterRequest{Name: "dev", Provider: "k3s"}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "kubernetes", config.KubernetesClusterFileToken("dev")+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	owner, err := clusterKubeconfigOwner("dev")
	if err != nil {
		t.Fatal(err)
	}
	if owner != (kubernetes.KubeconfigOwner{Cluster: "dev", Provider: "k3s"}) {
		t.Fatalf("owner = %+v", owner)
	}
}
