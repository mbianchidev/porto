package runtimes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/networking"
)

func TestNetworkingEnvironmentHelper(t *testing.T) {
	if os.Getenv("PORTO_TEST_NETWORK_CHILD") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("HTTP_PROXY")+"|"+os.Getenv("NO_PROXY"))
	os.Exit(0)
}

func TestExecRunnerAppliesConfiguredProxyWithoutMutatingParent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PORTO_HOME", root)
	t.Setenv("HTTP_PROXY", "http://inherited.example:8080")
	if err := networking.New(root).Save(networking.Config{Mode: "manual", HTTPProxy: "http://configured.example:8080", NoProxy: ".internal.example"}); err != nil {
		t.Fatal(err)
	}

	output, err := (ExecRunner{}).Run(context.Background(), Command{
		Name: os.Args[0], Args: []string{"-test.run=^TestNetworkingEnvironmentHelper$"},
		Env:     []string{"PORTO_TEST_NETWORK_CHILD=1"},
		Network: true,
	})
	if err != nil {
		t.Fatalf("run proxy probe: %v: %s", err, output)
	}
	if !strings.HasPrefix(string(output), "http://configured.example:8080|") ||
		!strings.Contains(string(output), ".internal.example") || !strings.Contains(string(output), ".localhost") {
		t.Fatalf("runtime proxy environment = %q", output)
	}
	if os.Getenv("HTTP_PROXY") != "http://inherited.example:8080" {
		t.Fatal("runtime proxy configuration mutated the parent environment")
	}
}

func TestRegistryOperationsRecognizeNativeAndDockerGlobalFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--address", `\\.\pipe\synthetic`, "--namespace", "porto-windows", "--snapshotter", "windows", "pull", "synthetic:v1"},
		{"--address=synthetic", "--data-root", "synthetic path", "--cni-path", "bin", "--cni-netconfpath", "cni", "image", "push", "synthetic:v1"},
		{"--context", "porto", "--config=synthetic", "buildx", "build", "."},
		{"--debug", "--hosts-dir", "synthetic", "pull", "synthetic:v1"},
	} {
		if !RegistryOperation(args) {
			t.Errorf("outbound operation was not recognized: %q", args)
		}
	}
	for _, args := range [][]string{
		{"--address", "pull", "ps"}, {"--namespace", "synthetic", "container", "inspect", "pull"},
		{"--unknown", "pull"}, {"--address"}, {"network", "create", "pull"},
	} {
		if RegistryOperation(args) {
			t.Errorf("local or invalid operation classified as outbound: %q", args)
		}
	}
}
