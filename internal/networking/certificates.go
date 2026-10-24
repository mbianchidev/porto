package networking

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaxRegistryCABytes = 1024 * 1024

type CertificateImport struct {
	Reference   string `json:"reference"`
	Fingerprint string `json:"fingerprint"`
	Subject     string `json:"subject"`
}

func validateRegistryCA(data []byte) ([]byte, string, error) {
	if len(data) == 0 || len(data) > MaxRegistryCABytes {
		return nil, "", errors.New("public registry CA input must be between one byte and one MiB")
	}
	rest := data
	var canonical []byte
	subject := ""
	for len(bytes.TrimSpace(rest)) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, "", errors.New("registry CA input must contain only public PEM certificates")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.IsCA || time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
			return nil, "", errors.New("registry CA is invalid, expired, or not a certificate authority")
		}
		if subject == "" {
			subject = certificate.Subject.String()
		}
		canonical = append(canonical, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
		rest = remaining
	}
	if len(canonical) == 0 {
		return nil, "", errors.New("registry CA input is empty")
	}
	return canonical, subject, nil
}

func (m *Manager) ImportCertificate(data []byte) (CertificateImport, error) {
	canonical, subject, err := validateRegistryCA(data)
	if err != nil {
		return CertificateImport{}, err
	}
	sum := sha256.Sum256(canonical)
	fingerprint := hex.EncodeToString(sum[:])
	directory := filepath.Join(m.root, "network-certificates")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return CertificateImport{}, err
	}
	if err := atomicWrite(filepath.Join(directory, fingerprint+".pem"), canonical); err != nil {
		return CertificateImport{}, err
	}
	return CertificateImport{Reference: "sha256:" + fingerprint, Fingerprint: fingerprint, Subject: subject}, nil
}

func (m *Manager) readRegistryCA(reference string) ([]byte, error) {
	digest, found := strings.CutPrefix(reference, "sha256:")
	decoded, err := hex.DecodeString(digest)
	if !found || err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != digest {
		return nil, errors.New("registry CA must be an imported sha256 reference, not a host file path")
	}
	root, err := os.OpenRoot(filepath.Join(m.root, "network-certificates"))
	if err != nil {
		return nil, fmt.Errorf("open owned registry certificate store: %w", err)
	}
	defer root.Close()
	data, err := root.ReadFile(hex.EncodeToString(decoded) + ".pem")
	if err != nil {
		return nil, fmt.Errorf("read imported registry CA: %w", err)
	}
	canonical, _, err := validateRegistryCA(data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	if !bytes.Equal(sum[:], decoded) {
		return nil, errors.New("imported registry CA content no longer matches its fingerprint")
	}
	return canonical, nil
}
