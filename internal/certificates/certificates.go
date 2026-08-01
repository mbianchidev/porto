package certificates

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	validity          = 365 * 24 * time.Hour
	authorityValidity = 10 * 365 * 24 * time.Hour
	renewalWindow     = 30 * 24 * time.Hour
)

var requiredDNSNames = []string{
	"porto.local",
	"*.porto.local",
	"porto.localhost",
	"*.porto.localhost",
	"localhost",
}

type Status struct {
	CertificatePath          string    `json:"certificatePath"`
	KeyPath                  string    `json:"keyPath"`
	CertificateAuthorityPath string    `json:"certificateAuthorityPath"`
	DNSNames                 []string  `json:"dnsNames"`
	NotBefore                time.Time `json:"notBefore"`
	NotAfter                 time.Time `json:"notAfter"`
	Fingerprint              string    `json:"fingerprint"`
}

type Manager struct {
	mu                       sync.RWMutex
	certificatePath          string
	keyPath                  string
	certificateAuthorityPath string
	authorityKeyPath         string
	certificate              *tls.Certificate
	leaf                     *x509.Certificate
	authority                *x509.Certificate
	authorityKey             crypto.Signer
	allowedDNSNames          map[string]struct{}
	dynamicCertificates      map[string]*tls.Certificate
}

func New(certificatePath, keyPath, certificateAuthorityPath, authorityKeyPath string) *Manager {
	return &Manager{
		certificatePath:          certificatePath,
		keyPath:                  keyPath,
		certificateAuthorityPath: certificateAuthorityPath,
		authorityKeyPath:         authorityKeyPath,
	}
}

func (m *Manager) Ensure(additionalDNSNames ...string) (Status, error) {
	lock, err := acquireFileLock(m.authorityKeyPath + ".lock")
	if err != nil {
		return Status{}, fmt.Errorf("lock TLS certificate files: %w", err)
	}
	defer lock.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setAllowedDNSNamesLocked(additionalDNSNames)

	if err := m.ensureAuthorityLocked(time.Now()); err != nil {
		return Status{}, err
	}
	if err := m.loadLocked(time.Now()); err == nil {
		return m.statusLocked(), nil
	}
	if err := m.generateLocked(time.Now()); err != nil {
		return Status{}, err
	}
	return m.statusLocked(), nil
}

func (m *Manager) Renew(additionalDNSNames ...string) (Status, error) {
	lock, err := acquireFileLock(m.authorityKeyPath + ".lock")
	if err != nil {
		return Status{}, fmt.Errorf("lock TLS certificate files: %w", err)
	}
	defer lock.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setAllowedDNSNamesLocked(additionalDNSNames)

	if err := m.ensureAuthorityLocked(time.Now()); err != nil {
		return Status{}, err
	}
	if err := m.generateLocked(time.Now()); err != nil {
		return Status{}, err
	}
	return m.statusLocked(), nil
}

func (m *Manager) Status() (Status, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.certificate == nil || m.leaf == nil {
		return Status{}, errors.New("TLS certificate is not loaded")
	}
	return m.statusLocked(), nil
}

func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.certificateForClient,
	}
}

func (m *Manager) certificateForClient(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	serverName := ""
	if hello != nil {
		serverName = normalizeDNSName(hello.ServerName)
	}
	m.mu.RLock()
	if m.certificate == nil || m.leaf == nil {
		m.mu.RUnlock()
		return nil, errors.New("TLS certificate is not loaded")
	}
	if serverName == "" || net.ParseIP(serverName) != nil || m.leaf.VerifyHostname(serverName) == nil {
		certificate := m.certificate
		m.mu.RUnlock()
		return certificate, nil
	}
	if _, allowed := m.allowedDNSNames[serverName]; !allowed {
		m.mu.RUnlock()
		return nil, fmt.Errorf("TLS certificate is not available for %q", serverName)
	}
	if certificate := m.dynamicCertificates[serverName]; certificate != nil {
		m.mu.RUnlock()
		return certificate, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if certificate := m.dynamicCertificates[serverName]; certificate != nil {
		return certificate, nil
	}
	if _, allowed := m.allowedDNSNames[serverName]; !allowed {
		return nil, fmt.Errorf("TLS certificate is not available for %q", serverName)
	}
	if m.authority == nil || m.authorityKey == nil {
		return nil, errors.New("TLS certificate authority is not loaded")
	}
	certificate, _, _, err := m.issueCertificateLocked(time.Now(), []string{serverName}, nil)
	if err != nil {
		return nil, err
	}
	m.dynamicCertificates[serverName] = certificate
	return certificate, nil
}

func (m *Manager) loadLocked(now time.Time) error {
	pair, err := tls.LoadX509KeyPair(m.certificatePath, m.keyPath)
	if err != nil {
		return err
	}
	if len(pair.Certificate) == 0 {
		return errors.New("TLS certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse TLS certificate: %w", err)
	}
	if err := validate(leaf, m.authority, now); err != nil {
		return err
	}
	pair.Leaf = leaf
	m.certificate = &pair
	m.leaf = leaf
	return nil
}

func (m *Manager) generateLocked(now time.Time) error {
	pair, certificatePEM, keyPEM, err := m.issueCertificateLocked(
		now,
		requiredDNSNames,
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	)
	if err != nil {
		return err
	}
	if err := writeAtomic(m.keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write TLS private key: %w", err)
	}
	if err := writeAtomic(m.certificatePath, certificatePEM, 0o644); err != nil {
		return fmt.Errorf("write TLS certificate: %w", err)
	}
	m.certificate = pair
	m.leaf = pair.Leaf
	return nil
}

func (m *Manager) issueCertificateLocked(now time.Time, dnsNames []string, ipAddresses []net.IP) (*tls.Certificate, []byte, []byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate TLS private key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate TLS certificate serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Porto Local Development",
			Organization: []string{"Porto"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              append([]string(nil), dnsNames...),
		IPAddresses:           append([]net.IP(nil), ipAddresses...),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, m.authority, &privateKey.PublicKey, m.authorityKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create TLS certificate: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal TLS private key: %w", err)
	}
	authorityPEM, err := os.ReadFile(m.certificateAuthorityPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read TLS certificate authority: %w", err)
	}
	certificatePEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), authorityPEM...)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load generated TLS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse generated TLS certificate: %w", err)
	}
	pair.Leaf = leaf
	return &pair, certificatePEM, keyPEM, nil
}

func (m *Manager) statusLocked() Status {
	sum := sha256.Sum256(m.leaf.Raw)
	return Status{
		CertificatePath:          m.certificatePath,
		KeyPath:                  m.keyPath,
		CertificateAuthorityPath: m.certificateAuthorityPath,
		DNSNames:                 append([]string(nil), m.leaf.DNSNames...),
		NotBefore:                m.leaf.NotBefore,
		NotAfter:                 m.leaf.NotAfter,
		Fingerprint:              hex.EncodeToString(sum[:]),
	}
}

func validate(certificate, authority *x509.Certificate, now time.Time) error {
	if now.Before(certificate.NotBefore) {
		return errors.New("TLS certificate is not valid yet")
	}
	if !certificate.NotAfter.After(now.Add(renewalWindow)) {
		return errors.New("TLS certificate is expired or near expiry")
	}
	if !slices.Equal(certificate.DNSNames, requiredDNSNames) {
		return errors.New("TLS certificate DNS names do not match the required base names")
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority)
	if _, err := certificate.Verify(x509.VerifyOptions{
		DNSName:     "porto.local",
		Roots:       roots,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		CurrentTime: now,
	}); err != nil {
		return fmt.Errorf("verify TLS certificate: %w", err)
	}
	return nil
}

func (m *Manager) ensureAuthorityLocked(now time.Time) error {
	if err := m.loadAuthorityLocked(now); err == nil {
		return nil
	}
	return m.generateAuthorityLocked(now)
}

func (m *Manager) loadAuthorityLocked(now time.Time) error {
	pair, err := tls.LoadX509KeyPair(m.certificateAuthorityPath, m.authorityKeyPath)
	if err != nil {
		return err
	}
	if len(pair.Certificate) == 0 {
		return errors.New("TLS certificate authority chain is empty")
	}
	authority, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse TLS certificate authority: %w", err)
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return errors.New("TLS certificate authority private key cannot sign certificates")
	}
	if !authority.IsCA || authority.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("TLS certificate authority cannot sign certificates")
	}
	if now.Before(authority.NotBefore) || !authority.NotAfter.After(now.Add(renewalWindow)) {
		return errors.New("TLS certificate authority is expired or near expiry")
	}
	if err := authority.CheckSignatureFrom(authority); err != nil {
		return fmt.Errorf("verify TLS certificate authority: %w", err)
	}
	m.authority = authority
	m.authorityKey = signer
	return nil
}

func (m *Manager) generateAuthorityLocked(now time.Time) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate TLS certificate authority key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return fmt.Errorf("generate TLS certificate authority serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Porto Local Development Root CA",
			Organization: []string{"Porto"},
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(authorityValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create TLS certificate authority: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal TLS certificate authority key: %w", err)
	}
	if err := writeAtomic(m.authorityKeyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), 0o600); err != nil {
		return fmt.Errorf("write TLS certificate authority key: %w", err)
	}
	if err := writeAtomic(m.certificateAuthorityPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return fmt.Errorf("write TLS certificate authority: %w", err)
	}
	if err := m.loadAuthorityLocked(now); err != nil {
		return fmt.Errorf("load generated TLS certificate authority: %w", err)
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, serialLimit)
}

func (m *Manager) setAllowedDNSNamesLocked(additional []string) {
	allowed := make(map[string]struct{}, len(additional))
	for _, name := range additional {
		name = normalizeDNSName(name)
		if name == "" {
			continue
		}
		allowed[name] = struct{}{}
	}
	m.allowedDNSNames = allowed
	m.dynamicCertificates = make(map[string]*tls.Certificate)
}

func normalizeDNSName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

func writeAtomic(path string, contents []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); err == nil && closeErr != nil {
				err = closeErr
			}
		}
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = file.Chmod(mode); err != nil {
		return err
	}
	if _, err = file.Write(contents); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	closed = true
	if err = os.Rename(tempPath, path); err == nil {
		return nil
	}
	if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	err = os.Rename(tempPath, path)
	return err
}
