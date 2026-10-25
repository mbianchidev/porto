package networking

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/certificates"
)

func TestRegistryTrustRequiresImportedReferencesAndRemainsReversible(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "synthetic-ca.pem")
	manager := certificates.New(filepath.Join(root, "leaf.pem"), filepath.Join(root, "leaf.key"), authority, authority+".key")
	if _, err := manager.Ensure(); err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(authority)
	if err != nil {
		t.Fatal(err)
	}
	network := New(filepath.Join(root, "configuration"))
	if err := network.Save(Config{Mode: "off", RegistryCAs: []string{authority}}); err == nil {
		t.Fatal("remote configuration accepted an arbitrary host path")
	}
	imported, err := network.ImportCertificate(pem)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(authority + ".key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := network.ImportCertificate(key); err == nil {
		t.Fatal("private key was accepted as public trust")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "synthetic registry") }))
	server.TLS = manager.TLSConfig()
	pair, err := server.TLS.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	server.TLS.Certificates = []tls.Certificate{*pair}
	server.StartTLS()
	defer server.Close()
	if err := network.Save(Config{Mode: "off", RegistryCAs: []string{imported.Reference}}); err != nil {
		t.Fatal(err)
	}
	transport, err := network.Transport()
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("registry trust did not use an explicit verified root pool")
	}
	client := &http.Client{Transport: transport}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if err := network.Save(Config{Mode: "off", RegistryCAs: []string{}}); err != nil {
		t.Fatal(err)
	}
	cleared, err := network.Transport()
	if err != nil {
		t.Fatal(err)
	}
	defer cleared.CloseIdleConnections()
	if cleared.TLSClientConfig != nil && cleared.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("removal disabled validation")
	}
	if _, err := (&http.Client{Transport: cleared}).Get(server.URL); err == nil {
		t.Fatal("removed imported CA remained trusted")
	}
}
