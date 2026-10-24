package networking

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type memorySecrets map[string]string

func (s memorySecrets) Set(_, account, value string) error    { s[account] = value; return nil }
func (s memorySecrets) Get(_, account string) (string, error) { return s[account], nil }
func (s memorySecrets) Delete(_, account string) error        { delete(s, account); return nil }

func TestProxyCredentialsStayInVaultAndLocalRoutesBypassProxy(t *testing.T) {
	root := t.TempDir()
	manager := New(root)
	manager.secrets = memorySecrets{}
	if err := manager.Save(Config{Mode: "manual", HTTPSProxy: "http://synthetic:mock-password@proxy.example:8080"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "networking.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "mock-password") || strings.Contains(string(data), "synthetic") {
		t.Fatalf("proxy credentials leaked into configuration: %s", data)
	}
	target, _ := url.Parse("https://registry.example/v2/")
	proxy, err := manager.ProxyForURL(target)
	if err != nil || proxy == nil || proxy.User.String() != "synthetic:mock-password" {
		t.Fatalf("resolved proxy = %v, %v", proxy, err)
	}
	for _, address := range []string{"https://api.porto.localhost/", "https://nested.app.porto.local/", "http://127.0.0.1:37623/", "https://[::1]/"} {
		target, _ := url.Parse(address)
		proxy, err := manager.ProxyForURL(target)
		if err != nil || proxy != nil {
			t.Fatalf("local endpoint %s used proxy %v: %v", address, proxy, err)
		}
	}
}
