package certificates

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExpiredAndNearExpiryLeavesRenewWithoutReplacingTrustedAuthority(t *testing.T) {
	for _, age := range []time.Duration{validity + time.Hour, validity - renewalWindow + time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			root := t.TempDir()
			certificate, key := filepath.Join(root, "leaf.pem"), filepath.Join(root, "leaf.key")
			authority, authorityKey := filepath.Join(root, "ca.pem"), filepath.Join(root, "ca.key")
			manager := New(certificate, key, authority, authorityKey)
			if _, err := manager.Ensure("nested.synthetic.porto.localhost"); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(authority)
			if err != nil {
				t.Fatal(err)
			}
			_, expired, expiredKey, err := manager.issueCertificateLocked(time.Now().Add(-age), requiredDNSNames, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(certificate, expired, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(key, expiredKey, 0o600); err != nil {
				t.Fatal(err)
			}
			restarted := New(certificate, key, authority, authorityKey)
			status, err := restarted.Ensure("nested.synthetic.porto.localhost")
			if err != nil || !status.NotAfter.After(time.Now().Add(renewalWindow)) {
				t.Fatalf("expired/near-expiry certificate was not recovered: %+v %v", status, err)
			}
			after, err := os.ReadFile(authority)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("leaf recovery replaced the trusted authority")
			}
			pair, err := restarted.TLSConfig().GetCertificate(&tls.ClientHelloInfo{ServerName: "nested.synthetic.porto.localhost"})
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			roots.AppendCertsFromPEM(before)
			if _, err := pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "nested.synthetic.porto.localhost"}); err != nil {
				t.Fatalf("recovered dotted SNI lost trust: %v", err)
			}
		})
	}
}
