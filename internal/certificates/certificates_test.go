package certificates

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureGeneratesAndReusesCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := New(filepath.Join(dir, "porto.local.pem"), filepath.Join(dir, "porto.local-key.pem"))

	first, err := manager.Ensure()
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}
	second, err := manager.Ensure()
	if err != nil {
		t.Fatalf("reuse certificate: %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("certificate was unexpectedly regenerated: %q != %q", first.Fingerprint, second.Fingerprint)
	}
	if !reflect.DeepEqual(first.DNSNames, requiredDNSNames) {
		t.Fatalf("DNS names = %v, want %v", first.DNSNames, requiredDNSNames)
	}

	pair, err := tls.LoadX509KeyPair(first.CertificatePath, first.KeyPath)
	if err != nil {
		t.Fatalf("load generated key pair: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	if err := leaf.VerifyHostname("app.porto.local"); err != nil {
		t.Fatalf("verify wildcard hostname: %v", err)
	}
}

func TestRenewReplacesLiveCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := New(filepath.Join(dir, "porto.local.pem"), filepath.Join(dir, "porto.local-key.pem"))
	first, err := manager.Ensure()
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}
	before, err := manager.TLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatalf("get certificate: %v", err)
	}

	second, err := manager.Renew()
	if err != nil {
		t.Fatalf("renew certificate: %v", err)
	}
	after, err := manager.TLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatalf("get renewed certificate: %v", err)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("renewal did not replace the certificate")
	}
	if before == after {
		t.Fatal("live TLS certificate pointer was not replaced")
	}
}

func TestTLSConfigServesWildcardCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := New(filepath.Join(dir, "porto.local.pem"), filepath.Join(dir, "porto.local-key.pem"))
	status, err := manager.Ensure()
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}
	certificatePEM, err := os.ReadFile(status.CertificatePath)
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("append generated certificate to trust pool")
	}

	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure"))
	}))
	server.TLS = manager.TLSConfig()
	server.StartTLS()
	defer server.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: "app.porto.local",
	}}}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("request TLS server: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read TLS response: %v", err)
	}
	if string(body) != "secure" {
		t.Fatalf("TLS response = %q", body)
	}
}
