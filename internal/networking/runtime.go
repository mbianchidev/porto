package networking

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RuntimeConfiguration crosses only the verified owner-to-worker private pipe.
// Proxy credentials are resolved by the owning user, never by LocalSystem.
type RuntimeConfiguration struct {
	Config     Config `json:"config"`
	CAPEM      []byte `json:"caPEM,omitempty"`
	Generation string `json:"generation"`
}

func (m *Manager) ExportRuntime() (RuntimeConfiguration, error) {
	configuration, _, err := m.resolved()
	if err != nil {
		return RuntimeConfiguration{}, err
	}
	configuration.RegistryCAs = nil
	configuration.ClearCredentials = false
	if configuration.Mode != "off" {
		configuration.Mode = "manual"
	}
	bundle, err := m.registryBundle()
	if err != nil {
		return RuntimeConfiguration{}, err
	}
	generation, err := m.Generation()
	return RuntimeConfiguration{Config: configuration, CAPEM: bundle, Generation: generation}, err
}

func NewRuntime(root string, configuration RuntimeConfiguration) (*Manager, error) {
	if configuration.Config.Mode != "manual" && configuration.Config.Mode != "off" {
		return nil, errors.New("native worker networking requires explicitly resolved manual or off settings")
	}
	if len(configuration.Config.RegistryCAs) != 0 || configuration.Config.ClearCredentials {
		return nil, errors.New("native worker networking accepts public CA bytes, not host references or vault operations")
	}
	if strings.ContainsAny(configuration.Config.NoProxy, "\x00\r\n") {
		return nil, errors.New("invalid native proxy bypass rules")
	}
	for _, value := range []string{configuration.Config.HTTPProxy, configuration.Config.HTTPSProxy, configuration.Config.SOCKSProxy} {
		if value != "" {
			if _, err := parseProxy(value); err != nil {
				return nil, err
			}
		}
	}
	if len(configuration.CAPEM) > 0 {
		canonical, _, err := validateRegistryCA(configuration.CAPEM)
		if err != nil {
			return nil, err
		}
		configuration.CAPEM = canonical
	}
	if configuration.Config.Mode == "off" {
		configuration.Config.HTTPProxy, configuration.Config.HTTPSProxy, configuration.Config.SOCKSProxy = "", "", ""
	}
	return &Manager{root: root, runtime: &configuration}, nil
}

func (m *Manager) RegistryHostsDirectory() (string, error) {
	bundle, err := m.registryBundle()
	if err != nil || len(bundle) == 0 {
		return "", err
	}
	sum := sha256.Sum256(bundle)
	root := filepath.Join(m.root, "network-certificates", "runtime-"+hex.EncodeToString(sum[:]))
	directory := filepath.Join(root, "_default")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("prepare native registry trust: %w", err)
	}
	certificate := filepath.Join(root, "ca.pem")
	if err := atomicWrite(certificate, bundle); err != nil {
		return "", err
	}
	if err := atomicWrite(filepath.Join(directory, "hosts.toml"), []byte("ca = ["+strconv.Quote(certificate)+"]\n")); err != nil {
		return "", err
	}
	return root, nil
}
