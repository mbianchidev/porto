package localhttps

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

type Status struct {
	Installed      bool   `json:"installed"`
	Listening      bool   `json:"listening"`
	Trusted        bool   `json:"trusted"`
	Supported      bool   `json:"supported"`
	Message        string `json:"message,omitempty"`
	BrowserTrusted bool   `json:"browserTrusted"`
}

type installSteps struct {
	installed, trusted bool
	install, remove    func() error
	trust, untrust     func() error
	verify             func() bool
	mark               func() error
}

func installWithRollback(steps installSteps) (err error) {
	helperInstalled, trustAttempted := false, false
	defer func() {
		if err == nil {
			return
		}
		if trustAttempted && !steps.trusted {
			err = errors.Join(err, steps.untrust())
		}
		if helperInstalled && !steps.installed {
			err = errors.Join(err, steps.remove())
		}
	}()
	if err = steps.install(); err != nil {
		return err
	}
	helperInstalled = true
	if !steps.trusted {
		trustAttempted = true
		if err = steps.trust(); err != nil {
			return fmt.Errorf("trust Porto certificate authority: %w", err)
		}
	}
	if !steps.verify() {
		return errors.New("Porto certificate is not trusted or is expired; renew it and retry 'porto https install'")
	}
	return steps.mark()
}

func markInstalled() error {
	path, err := config.PortlessHTTPSMarkerPath()
	if err != nil {
		return err
	}
	if err := writeIntegrationFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("record portless HTTPS installation: %w", err)
	}
	return nil
}

func removeMarker() error {
	path, err := config.PortlessHTTPSMarkerPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove portless HTTPS marker: %w", err)
	}
	return nil
}

func listenersReady() bool {
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		connection, err := net.DialTimeout("tcp", address, 300*time.Millisecond)
		if err != nil {
			return false
		}
		_ = connection.Close()
	}
	return true
}

func checkPortsAvailable() error {
	var listeners []net.Listener
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	for _, address := range []string{ListenAddress, ListenAddressIPv6} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return fmt.Errorf("HTTPS port %s is unavailable; stop the conflicting listener or enable IPv6 loopback, then retry: %w", address, err)
		}
		listeners = append(listeners, listener)
	}
	return nil
}

func readAuthority(path string) ([]byte, string, error) {
	data, fingerprint, err := readAuthorityIdentity(path)
	if err != nil {
		return nil, "", err
	}
	block, _ := pem.Decode(data)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	if now.Before(certificate.NotBefore) || !now.Before(certificate.NotAfter) {
		return nil, "", errors.New("Porto certificate authority is not yet valid or is expired")
	}
	return data, fingerprint, nil
}

func readAuthorityIdentity(path string) ([]byte, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read Porto certificate authority: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, "", errors.New("Porto certificate authority must be a PEM certificate")
	}
	if block, _ := pem.Decode(rest); block != nil {
		return nil, "", errors.New("Porto certificate authority must contain exactly one certificate and no private keys")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("parse Porto certificate authority: %w", err)
	}
	if !certificate.IsCA || certificate.CheckSignatureFrom(certificate) != nil {
		return nil, "", errors.New("Porto certificate authority must be a self-signed CA")
	}
	sum := sha256.Sum256(certificate.Raw)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), hex.EncodeToString(sum[:]), nil
}

func verifySystemTrust(certificatePath string) bool {
	data, err := os.ReadFile(certificatePath)
	if err != nil {
		return false
	}
	block, rest := pem.Decode(data)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	intermediates := x509.NewCertPool()
	intermediates.AppendCertsFromPEM(rest)
	_, err = leaf.Verify(x509.VerifyOptions{
		DNSName: config.LocalhostDomain, Intermediates: intermediates,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

func writeIntegrationFile(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular integration file %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".porto-https-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
