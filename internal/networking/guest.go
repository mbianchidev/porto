package networking

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type GuestConfig struct {
	Generation    string
	Environment   []string
	RegistryCAPEM []byte
}

func (m *Manager) Generation() (string, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "networking.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (m *Manager) registryBundle() ([]byte, error) {
	stored, err := m.load()
	if err != nil {
		return nil, err
	}
	var bundle []byte
	for _, reference := range stored.RegistryCAs {
		data, err := m.readRegistryCA(reference)
		if err != nil {
			return nil, err
		}
		bundle = append(bundle, data...)
		bundle = append(bundle, '\n')
	}
	return bundle, nil
}

func (m *Manager) GuestConfiguration() (GuestConfig, error) {
	generation, err := m.Generation()
	if err != nil {
		return GuestConfig{}, err
	}
	if generation == "" {
		return GuestConfig{}, errors.New("save networking settings before applying them to the engine")
	}
	environment, err := m.Environment()
	if err != nil {
		return GuestConfig{}, err
	}
	guest := GuestConfig{Generation: generation}
	for _, variable := range environment {
		name, value, _ := strings.Cut(variable, "=")
		if strings.EqualFold(name, "SSL_CERT_FILE") {
			continue
		}
		if strings.HasSuffix(strings.ToUpper(name), "_PROXY") && !strings.EqualFold(name, "NO_PROXY") && value != "" {
			proxy, err := parseProxy(value)
			if err != nil {
				return GuestConfig{}, err
			}
			host := proxy.Hostname()
			ip := net.ParseIP(host)
			if strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
				if proxy.Scheme == "https" {
					return GuestConfig{}, errors.New("a TLS-encrypted loopback proxy needs a guest-reachable hostname covered by its certificate; Porto will not disable proxy certificate validation")
				}
				port := proxy.Port()
				proxy.Host = "host.lima.internal"
				if port != "" {
					proxy.Host = net.JoinHostPort(proxy.Host, port)
				}
				value = proxy.String()
			}
		}
		guest.Environment = append(guest.Environment, name+"="+value)
	}
	guest.RegistryCAPEM, err = m.registryBundle()
	return guest, err
}

func (m *Manager) hostRegistryEnvironment() ([]string, error) {
	bundle, err := m.registryBundle()
	if err != nil || len(bundle) == 0 {
		return nil, err
	}
	path := filepath.Join(m.root, "network-certificates", "runtime-roots.pem")
	if inherited := os.Getenv("SSL_CERT_FILE"); inherited != "" && inherited != path {
		existing, err := os.ReadFile(inherited)
		if err != nil {
			return nil, fmt.Errorf("preserve configured SSL_CERT_FILE while adding registry trust: %w", err)
		}
		bundle = append(append(existing, '\n'), bundle...)
	}
	if err := atomicWrite(path, bundle); err != nil {
		return nil, err
	}
	return []string{"SSL_CERT_FILE=" + path}, nil
}
