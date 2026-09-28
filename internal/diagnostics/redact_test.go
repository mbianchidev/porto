package diagnostics

import (
	"strings"
	"testing"
)

func TestRedactRemovesCommonSecretsAndPrivateMaterial(t *testing.T) {
	input := `{
  "username": "porto-user",
  "password": "super-secret-password",
  "token": "ghp_very-secret-token",
  "auth": "dXNlcjpwYXNz",
  "client-key-data": "private-kube-key",
  "certificate-authority-data": "private-kube-ca"
}
REGISTRY_PASSWORD=registry-secret
HTTPS_PROXY=https://proxy-user:proxy-password@example.test:8443
Authorization: Bearer bearer-secret
-----BEGIN PRIVATE KEY-----
private-key-body
-----END PRIVATE KEY-----`

	output, redactions := Redact([]byte(input), RedactionContext{
		HomeDirectory: "/Users/example",
		Hostname:      "private-host",
		Username:      "example",
	})
	text := string(output)
	for _, secret := range []string{
		"super-secret-password",
		"ghp_very-secret-token",
		"dXNlcjpwYXNz",
		"private-kube-key",
		"private-kube-ca",
		"registry-secret",
		"proxy-password",
		"bearer-secret",
		"private-key-body",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("redacted output exposed %q:\n%s", secret, text)
		}
	}
	if redactions < 8 || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("redactions = %d, output = %s", redactions, text)
	}
}

func TestRedactReplacesHostIdentityAndHomeDirectory(t *testing.T) {
	output, _ := Redact(
		[]byte("host=private-host path=/Users/example/work/project user=example"),
		RedactionContext{
			HomeDirectory: "/Users/example",
			Hostname:      "private-host",
			Username:      "example",
		},
	)
	text := string(output)
	if strings.Contains(text, "private-host") || strings.Contains(text, "/Users/example") {
		t.Fatalf("host identity was not redacted: %s", text)
	}
	if !strings.Contains(text, "$HOME") || !strings.Contains(text, "$HOSTNAME") {
		t.Fatalf("redacted placeholders missing: %s", text)
	}
}
