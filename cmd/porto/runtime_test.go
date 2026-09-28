package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
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
