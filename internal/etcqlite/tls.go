package etcqlite

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

func serial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func pemKey(key *ecdsa.PrivateKey) ([]byte, error) {
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), nil
}

func (m *Manager) createCertificates(instance Instance) error {
	directory := filepath.Join(m.instancePath(instance.Name), "tls")
	if err := privateDirectory(directory); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	number, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	authority := &x509.Certificate{
		SerialNumber: number, Subject: pkix.Name{CommonName: "Porto Etcqlite " + instance.ClusterID},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(5, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, authority, authority, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyPEM, err := pemKey(key)
	if err != nil {
		return err
	}
	for name, contents := range map[string][]byte{
		"ca.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "ca-key.pem": keyPEM,
	} {
		if err := writeExclusive(filepath.Join(directory, name), contents); err != nil {
			return fmt.Errorf("write private Etcqlite authority: %w", err)
		}
	}
	issue := func(name string, hosts []string, identity *url.URL) error {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		number, err := serial()
		if err != nil {
			return err
		}
		leaf := &x509.Certificate{
			SerialNumber: number, Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(1, 0, 0),
			KeyUsage:    x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		if len(hosts) > 0 {
			leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		}
		for _, host := range hosts {
			if ip := net.ParseIP(host); ip != nil {
				leaf.IPAddresses = append(leaf.IPAddresses, ip)
			} else {
				leaf.DNSNames = append(leaf.DNSNames, host)
			}
		}
		if identity != nil {
			leaf.URIs = []*url.URL{identity}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, authority, &leafKey.PublicKey, key)
		if err != nil {
			return err
		}
		private, err := pemKey(leafKey)
		if err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(directory, name+"-key.pem"), private); err != nil {
			return err
		}
		return writeExclusive(filepath.Join(directory, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	if err := issue("client", nil, nil); err != nil {
		return err
	}
	for _, member := range instance.Members {
		endpoint, err := url.Parse(member.ClientURL)
		if err != nil {
			return err
		}
		tag, err := identityETag(instance.ClusterID, member.ID)
		if err != nil {
			return err
		}
		identity, err := url.Parse("spiffe://etcqlite/" + tag[1:17] + "/" + tag[18:34])
		if err != nil {
			return err
		}
		if err := issue(member.Name, []string{endpoint.Hostname(), "127.0.0.1", "::1"}, identity); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) tlsMaterial(instance Instance) (Connection, error) {
	result := Connection{ClusterID: instance.ClusterID}
	directory := filepath.Join(m.instancePath(instance.Name), "tls")
	var err error
	result.CA, err = os.ReadFile(filepath.Join(directory, "ca.pem"))
	if err != nil {
		return result, err
	}
	result.Certificate, err = os.ReadFile(filepath.Join(directory, "client.pem"))
	if err != nil {
		return result, err
	}
	result.Key, err = readPrivateKey(filepath.Join(directory, "client-key.pem"))
	if err != nil {
		return result, err
	}
	for _, member := range instance.Members {
		result.Endpoints = append(result.Endpoints, member.ClientURL)
	}
	data, err := json.Marshal(struct {
		ClusterID string
		Endpoints []string
		CA        []byte
	}{instance.ClusterID, result.Endpoints, result.CA})
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(data)
	result.Fingerprint = hex.EncodeToString(sum[:])
	return result, nil
}

func readPrivateKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("Etcqlite private key must be a private regular file")
	}
	return os.ReadFile(path)
}

func (m *Manager) client(instance Instance, member Member) (*http.Client, string, error) {
	material, err := m.tlsMaterial(instance)
	if err != nil {
		return nil, "", fmt.Errorf("load Etcqlite client mTLS material: %w", err)
	}
	pair, err := tls.X509KeyPair(material.Certificate, material.Key)
	if err != nil {
		return nil, "", err
	}
	authority := x509.NewCertPool()
	if !authority.AppendCertsFromPEM(material.CA) {
		return nil, "", errors.New("Etcqlite CA is invalid")
	}
	endpoint, err := url.Parse(member.ClientURL)
	if err != nil {
		return nil, "", err
	}
	host, port, err := net.SplitHostPort(member.Listen)
	if err != nil {
		return nil, "", err
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.To4() == nil {
			host = "::1"
		}
	}
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: authority,
			Certificates: []tls.Certificate{pair}, ServerName: endpoint.Hostname(),
		},
		DisableKeepAlives: true, ResponseHeaderTimeout: 10 * time.Second,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Etcqlite native requests must not redirect")
		},
	}
	return client, "https://" + net.JoinHostPort(host, port), nil
}

func (m *Manager) probe(ctx context.Context, instance Instance, member Member) (Discovery, error) {
	client, base, err := m.client(instance, member)
	if err != nil {
		return Discovery{}, err
	}
	probeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	discovery, err := nativeDiscovery(probeContext, client, base, m.token(), instance.ClusterID, member.ID)
	if err != nil {
		return discovery, err
	}
	return discovery, nativeReady(probeContext, client, base, m.token())
}
