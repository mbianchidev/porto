package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
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
