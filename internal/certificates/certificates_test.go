package certificates

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestEnsureGeneratesAndReusesCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)

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

func TestEnsureServesExactDottedProjectHostnameSeparately(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
	status, err := manager.Ensure("devoidofbeauty.com.porto.local")
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}
	if !reflect.DeepEqual(status.DNSNames, requiredDNSNames) {
		t.Fatalf("base DNS names = %v, want %v", status.DNSNames, requiredDNSNames)
	}

	pair, err := manager.TLSConfig().GetCertificate(&tls.ClientHelloInfo{
		ServerName: "devoidofbeauty.com.porto.local",
	})
	if err != nil {
		t.Fatalf("get dotted hostname certificate: %v", err)
	}
	if !reflect.DeepEqual(pair.Leaf.DNSNames, []string{"devoidofbeauty.com.porto.local"}) {
		t.Fatalf("dotted certificate DNS names = %v", pair.Leaf.DNSNames)
	}
	if err := pair.Leaf.VerifyHostname("devoidofbeauty.com.porto.local"); err != nil {
		t.Fatalf("verify dotted project hostname: %v", err)
	}
}

func TestEnsureDoesNotRegenerateBaseCertificateForNewHostname(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
	first, err := manager.Ensure()
	if err != nil {
		t.Fatalf("ensure initial certificate: %v", err)
	}
	second, err := manager.Ensure("devoidofbeauty.com.porto.local")
	if err != nil {
		t.Fatalf("ensure expanded certificate: %v", err)
	}
	if first.Fingerprint != second.Fingerprint {
		t.Fatal("base certificate was regenerated for a dynamic hostname")
	}
	if !reflect.DeepEqual(second.DNSNames, requiredDNSNames) {
		t.Fatalf("base DNS names = %v, want %v", second.DNSNames, requiredDNSNames)
	}
}

func TestRenewReplacesLiveCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
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
	authorityBefore, err := os.ReadFile(first.CertificateAuthorityPath)
	if err != nil {
		t.Fatal(err)
	}
	authorityAfter, err := os.ReadFile(second.CertificateAuthorityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(authorityBefore, authorityAfter) {
		t.Fatal("renewal replaced the trusted certificate authority")
	}
}

func TestTLSConfigServesWildcardCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
	status, err := manager.Ensure()
	if err != nil {
		t.Fatalf("ensure certificate: %v", err)
	}

	certificatePEM, err := os.ReadFile(status.CertificateAuthorityPath)
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

func TestTLSConfigUsesBaseCertificateForSingleLabelHostname(t *testing.T) {
	manager := testManager(t.TempDir())
	if _, err := manager.Ensure("devoidofbeauty.com.porto.localhost"); err != nil {
		t.Fatal(err)
	}
	base, err := manager.TLSConfig().GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	singleLabel, err := manager.TLSConfig().GetCertificate(&tls.ClientHelloInfo{
		ServerName: "2dnd.porto.localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if base != singleLabel {
		t.Fatal("single-label hostname did not use the base wildcard certificate")
	}
}

func TestTLSConfigRejectsUnknownDottedHostname(t *testing.T) {
	manager := testManager(t.TempDir())
	if _, err := manager.Ensure("allowed.example.porto.localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.TLSConfig().GetCertificate(&tls.ClientHelloInfo{
		ServerName: "unknown.example.porto.localhost",
	}); err == nil {
		t.Fatal("unknown dotted hostname received a certificate")
	}
}

func TestTLSConfigCachesConcurrentDottedHostnameCertificate(t *testing.T) {
	manager := testManager(t.TempDir())
	const hostname = "allowed.example.porto.localhost"
	if _, err := manager.Ensure(hostname); err != nil {
		t.Fatal(err)
	}
	const callers = 20
	certificates := make(chan *tls.Certificate, callers)
	errors := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			certificate, err := manager.TLSConfig().GetCertificate(&tls.ClientHelloInfo{
				ServerName: hostname,
			})
			certificates <- certificate
			errors <- err
		}()
	}
	wait.Wait()
	close(certificates)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *tls.Certificate
	for certificate := range certificates {
		if first == nil {
			first = certificate
			continue
		}
		if certificate != first {
			t.Fatal("concurrent requests received different cached certificates")
		}
	}
}

func TestEnsureReplacesOverbroadBaseCertificate(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
	if _, err := manager.Ensure(); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	overbroadNames := append(append([]string(nil), requiredDNSNames...), "devoidofbeauty.com.porto.localhost")
	overbroad, certificatePEM, keyPEM, err := manager.issueCertificateLocked(
		time.Now(),
		overbroadNames,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	)
	manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manager.keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(manager.certificatePath, certificatePEM, 0o644); err != nil {
		t.Fatal(err)
	}

	reloaded := testManager(dir)
	status, err := reloaded.Ensure("devoidofbeauty.com.porto.localhost")
	if err != nil {
		t.Fatal(err)
	}
	if status.Fingerprint == fingerprint(overbroad.Leaf) {
		t.Fatal("overbroad base certificate was reused")
	}
	if !reflect.DeepEqual(status.DNSNames, requiredDNSNames) {
		t.Fatalf("base DNS names = %v, want %v", status.DNSNames, requiredDNSNames)
	}
}

func TestCertificateIsSignedByPersistentAuthority(t *testing.T) {
	dir := t.TempDir()
	manager := testManager(dir)
	status, err := manager.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(status.CertificatePath, status.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	authorityPEM, err := os.ReadFile(status.CertificateAuthorityPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(authorityPEM)
	if block == nil {
		t.Fatal("decode certificate authority")
	}
	authority, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !authority.IsCA {
		t.Fatal("certificate authority is not marked as a CA")
	}
	if err := leaf.CheckSignatureFrom(authority); err != nil {
		t.Fatalf("leaf is not signed by authority: %v", err)
	}
}

func testManager(dir string) *Manager {
	return New(
		filepath.Join(dir, "porto.local.pem"),
		filepath.Join(dir, "porto.local-key.pem"),
		filepath.Join(dir, "porto-root-ca.pem"),
		filepath.Join(dir, "porto-root-ca-key.pem"),
	)
}

func fingerprint(certificate *x509.Certificate) string {
	sum := sha256.Sum256(certificate.Raw)
	return hex.EncodeToString(sum[:])
}
