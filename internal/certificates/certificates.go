package certificates

import (
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
	"sync"
	"time"
)

const (
	validity      = 365 * 24 * time.Hour
	renewalWindow = 30 * 24 * time.Hour
)

var requiredDNSNames = []string{
	"porto.local",
	"*.porto.local",
	"porto.localhost",
	"*.porto.localhost",
	"localhost",
}

type Status struct {
	CertificatePath string    `json:"certificatePath"`
	KeyPath         string    `json:"keyPath"`
	DNSNames        []string  `json:"dnsNames"`
	NotBefore       time.Time `json:"notBefore"`
	NotAfter        time.Time `json:"notAfter"`
	Fingerprint     string    `json:"fingerprint"`
}

type Manager struct {
	mu              sync.RWMutex
	certificatePath string
	keyPath         string
	certificate     *tls.Certificate
	leaf            *x509.Certificate
}

func New(certificatePath, keyPath string) *Manager {
	return &Manager{certificatePath: certificatePath, keyPath: keyPath}
}

func (m *Manager) Ensure() (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.loadLocked(time.Now()); err == nil {
		return m.statusLocked(), nil
	}
	if err := m.generateLocked(time.Now()); err != nil {
		return Status{}, err
	}
	return m.statusLocked(), nil
}

func (m *Manager) Renew() (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			m.mu.RLock()
			defer m.mu.RUnlock()
			if m.certificate == nil {
				return nil, errors.New("TLS certificate is not loaded")
			}
			return m.certificate, nil
		},
	}
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
	if err := validate(leaf, now); err != nil {
		return err
	}
	pair.Leaf = leaf
	m.certificate = &pair
	m.leaf = leaf
	return nil
}

func (m *Manager) generateLocked(now time.Time) error {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate TLS private key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return fmt.Errorf("generate TLS certificate serial: %w", err)
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
		DNSNames:              append([]string(nil), requiredDNSNames...),
		IPAddresses: []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create TLS certificate: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal TLS private key: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	if err := writeAtomic(m.keyPath, keyPEM, 0o600); err != nil {
		return fmt.Errorf("write TLS private key: %w", err)
	}
	if err := writeAtomic(m.certificatePath, certificatePEM, 0o644); err != nil {
		return fmt.Errorf("write TLS certificate: %w", err)
	}
	if err := m.loadLocked(now); err != nil {
		return fmt.Errorf("load generated TLS certificate: %w", err)
	}
	return nil
}

func (m *Manager) statusLocked() Status {
	sum := sha256.Sum256(m.leaf.Raw)
	return Status{
		CertificatePath: m.certificatePath,
		KeyPath:         m.keyPath,
		DNSNames:        append([]string(nil), m.leaf.DNSNames...),
		NotBefore:       m.leaf.NotBefore,
		NotAfter:        m.leaf.NotAfter,
		Fingerprint:     hex.EncodeToString(sum[:]),
	}
}

func validate(certificate *x509.Certificate, now time.Time) error {
	if now.Before(certificate.NotBefore) {
		return errors.New("TLS certificate is not valid yet")
	}
	if !certificate.NotAfter.After(now.Add(renewalWindow)) {
		return errors.New("TLS certificate is expired or near expiry")
	}
	for _, name := range requiredDNSNames {
		found := false
		for _, certificateName := range certificate.DNSNames {
			if certificateName == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("TLS certificate is missing DNS name %q", name)
		}
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
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
