package networking

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeNetworkingResolvesOwnerVaultWithoutWorkerUserConfiguration(t *testing.T) {
	owner := New(t.TempDir())
	owner.secrets = memorySecrets{}
	if err := owner.Save(Config{Mode: "manual", HTTPSProxy: "http://synthetic:mock-password@proxy.example.test:3128", NoProxy: ".internal.test"}); err != nil {
		t.Fatal(err)
	}
	configuration, err := owner.ExportRuntime()
	if err != nil {
		t.Fatal(err)
	}
	workerRoot := t.TempDir()
	worker, err := NewRuntime(workerRoot, configuration)
	if err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse("https://registry.example.test/v2/")
	proxy, err := worker.ProxyForURL(target)
	if err != nil || proxy == nil || proxy.User.String() != "synthetic:mock-password" {
		t.Fatalf("worker lost owner-resolved networking: %v %v", proxy, err)
	}
	environment, err := worker.Environment()
	if err != nil || !strings.Contains(strings.Join(environment, "\n"), "HTTPS_PROXY=http://synthetic:mock-password@proxy.example.test:3128") {
		t.Fatalf("worker subprocess policy: %q %v", environment, err)
	}
	for _, root := range []string{owner.root, workerRoot} {
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(path)
			if err == nil && strings.Contains(string(data), "mock-password") {
				t.Fatalf("plaintext proxy credential written at %s", path)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeNetworkingRejectsUnresolvedOrInvalidOwnerInput(t *testing.T) {
	for _, configuration := range []RuntimeConfiguration{
		{Config: Config{Mode: "system"}},
		{Config: Config{Mode: "manual", RegistryCAs: []string{"/foreign/host.pem"}}},
		{Config: Config{Mode: "manual", ClearCredentials: true}},
		{Config: Config{Mode: "manual", HTTPSProxy: "file:///foreign"}},
		{Config: Config{Mode: "manual", NoProxy: "host\ninjected"}},
		{Config: Config{Mode: "manual"}, CAPEM: []byte("not a public CA")},
	} {
		if _, err := NewRuntime(t.TempDir(), configuration); err == nil {
			t.Fatal("invalid worker networking was accepted")
		}
	}
}
