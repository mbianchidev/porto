package registries

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

type memorySecrets struct {
	values map[string]string
}

func (m *memorySecrets) Set(service, account, secret string) error {
	m.values[service+":"+account] = secret
	return nil
}

func (m *memorySecrets) Get(service, account string) (string, error) {
	value, ok := m.values[service+":"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (m *memorySecrets) Delete(service, account string) error {
	key := service + ":" + account
	if _, ok := m.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.values, key)
	return nil
}

func TestVaultStoresCredentialsOutsideRegistryMetadata(t *testing.T) {
	backend := &memorySecrets{values: map[string]string{}}
	vault := &Vault{backend: backend}
	if err := vault.Set(42, "test-token"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Get(42)
	if err != nil {
		t.Fatal(err)
	}
	if secret != "test-token" {
		t.Fatalf("secret = %q, want test-token", secret)
	}
	if err := vault.Delete(42); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Get(42); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("missing credential error = %v", err)
	}
}

func TestRegistryNormalizationAndImageMatching(t *testing.T) {
	for input, want := range map[string]string{
		"docker.io":                   DockerHubServer,
		"https://index.docker.io/v1/": DockerHubServer,
		"GHCR.IO":                     "ghcr.io",
		"registry.example.com:5000":   "registry.example.com:5000",
	} {
		got, err := NormalizeServer(input)
		if err != nil {
			t.Fatalf("NormalizeServer(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("NormalizeServer(%q) = %q, want %q", input, got, want)
		}
	}
	if err := ValidateImageServer("ghcr.io", "ghcr.io/example/private:latest"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateImageServer("ghcr.io", "alpine:latest"); err == nil {
		t.Fatal("Docker Hub test image matched GitHub Container Registry")
	}
}

func TestDockerConfigScopesConfiguredCredentials(t *testing.T) {
	config, err := DockerConfig([]Credential{
		{Server: "ghcr.io", Username: "octocat", Secret: "test-token"},
		{Server: "docker.io", Username: "docker-user", Secret: "docker-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(config, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Auths) != 2 {
		t.Fatalf("auth entries = %d, want 2", len(document.Auths))
	}
	decoded, err := base64.StdEncoding.DecodeString(document.Auths["ghcr.io"].Auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != "octocat:test-token" {
		t.Fatalf("GitHub registry auth = %q", decoded)
	}
}
