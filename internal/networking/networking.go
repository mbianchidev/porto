package networking

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/filelock"
	"github.com/zalando/go-keyring"
	"golang.org/x/net/http/httpproxy"
)

const (
	secretService = "dev.mbianchi.porto.network"
	localBypass   = "localhost,127.0.0.0/8,::1,.localhost,.porto.local"
)

var configMu sync.Mutex
var credentialURL = regexp.MustCompile(`(?i)(https?|socks5h?)://[^\s/@]+@`)

type Config struct {
	Mode             string   `json:"mode"`
	HTTPProxy        string   `json:"httpProxy"`
	HTTPSProxy       string   `json:"httpsProxy"`
	SOCKSProxy       string   `json:"socksProxy"`
	NoProxy          string   `json:"noProxy"`
	RegistryCAs      []string `json:"registryCAs"`
	ClearCredentials bool     `json:"clearCredentials,omitempty"`
}

type storedConfig struct {
	Config
	CredentialRefs map[string]string `json:"credentialRefs,omitempty"`
	configured     bool
}

type Status struct {
	Config
	Source            string          `json:"source"`
	CredentialsStored map[string]bool `json:"credentialsStored"`
	Message           string          `json:"message,omitempty"`
}

type secretStore = credentials.Store
type systemSecrets = credentials.System

type Manager struct {
	root    string
	secrets secretStore
	system  func() (Config, string, error)
}

func New(root string) *Manager {
	return &Manager{root: root, secrets: systemSecrets{}, system: systemProxy}
}

func Default() (*Manager, error) {
	root, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return New(root), nil
}

func (m *Manager) load() (storedConfig, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "networking.json"))
	if errors.Is(err, os.ErrNotExist) {
		return storedConfig{Config: Config{Mode: "system", RegistryCAs: []string{}}, CredentialRefs: map[string]string{}}, nil
	}
	if err != nil {
		return storedConfig{}, fmt.Errorf("read networking configuration: %w", err)
	}
	var configuration storedConfig
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return storedConfig{}, fmt.Errorf("decode networking configuration: %w", err)
	}
	if configuration.CredentialRefs == nil {
		configuration.CredentialRefs = map[string]string{}
	}
	if configuration.RegistryCAs == nil {
		configuration.RegistryCAs = []string{}
	}
	configuration.configured = true
	return configuration, nil
}

func (m *Manager) Save(configuration Config) (err error) {
	configMu.Lock()
	defer configMu.Unlock()
	lockContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	lease, err := filelock.Acquire(lockContext, filepath.Join(m.root, "networking.lock"))
	if err != nil {
		return fmt.Errorf("lock networking settings: %w", err)
	}
	defer func() { err = errors.Join(err, lease.Close()) }()
	switch configuration.Mode {
	case "system", "manual", "off":
	default:
		return errors.New("proxy mode must be system, manual, or off")
	}
	if strings.ContainsAny(configuration.NoProxy, "\x00\r\n") {
		return errors.New("proxy bypass rules must be comma-separated on one line")
	}
	previous, err := m.load()
	if err != nil {
		return err
	}
	next := storedConfig{Config: configuration, CredentialRefs: map[string]string{}}
	var createdSecrets []string
	defer func() {
		if err != nil {
			for _, account := range createdSecrets {
				err = errors.Join(err, m.secrets.Delete(secretService, account))
			}
		}
	}()
	proxies := map[string]*string{"http": &next.HTTPProxy, "https": &next.HTTPSProxy, "socks": &next.SOCKSProxy}
	oldProxies := map[string]string{"http": previous.HTTPProxy, "https": previous.HTTPSProxy, "socks": previous.SOCKSProxy}
	for name, value := range proxies {
		*value = strings.TrimSpace(*value)
		if *value == "" {
			continue
		}
		proxy, err := parseProxy(*value)
		if err != nil {
			return fmt.Errorf("invalid %s proxy: %w", name, err)
		}
		if proxy.User != nil {
			if configuration.ClearCredentials {
				return errors.New("clear saved proxy credentials without supplying replacement URL credentials")
			}
			secret := proxy.User.String()
			proxy.User = nil
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return err
			}
			account := "proxy-" + hex.EncodeToString(random[:])
			if err := m.secrets.Set(secretService, account, secret); err != nil {
				return fmt.Errorf("store proxy credentials in the native credential vault: %w", err)
			}
			createdSecrets = append(createdSecrets, account)
			next.CredentialRefs[name] = account
		} else if proxy.String() == oldProxies[name] && !configuration.ClearCredentials {
			next.CredentialRefs[name] = previous.CredentialRefs[name]
		}
		*value = proxy.String()
	}
	next.ClearCredentials = false
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return err
	}
	next.RegistryCAs = make([]string, 0, len(configuration.RegistryCAs))
	for _, path := range configuration.RegistryCAs {
		data, err := readRegistryCA(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		directory := filepath.Join(m.root, "network-certificates")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		destination := filepath.Join(directory, hex.EncodeToString(sum[:])+".pem")
		if err := atomicWrite(destination, data); err != nil {
			return err
		}
		next.RegistryCAs = append(next.RegistryCAs, destination)
	}
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(m.root, "networking.json"), append(data, '\n')); err != nil {
		return err
	}
	createdSecrets = nil
	var cleanupErr error
	for name, account := range previous.CredentialRefs {
		if account != "" && account != next.CredentialRefs[name] {
			if err := m.secrets.Delete(secretService, account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("network configuration was saved, but obsolete proxy credentials need vault cleanup: %w", err))
			}
		}
	}
	return cleanupErr
}

func parseProxy(value string) (*url.URL, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return nil, errors.New("proxy URL contains control characters")
	}
	proxy, err := url.Parse(value)
	if err != nil || proxy.Hostname() == "" || (proxy.Path != "" && proxy.Path != "/") || proxy.RawQuery != "" || proxy.Fragment != "" {
		return nil, errors.New("proxy must be an absolute URL without a path, query, or fragment")
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("proxy scheme must be http, https, socks5, or socks5h")
	}
	return proxy, nil
}

func (m *Manager) resolved() (Config, string, error) {
	stored, err := m.load()
	if err != nil {
		return Config{}, "", err
	}
	if !stored.configured {
		configuration, _ := environmentProxy()
		return configuration, "inherited environment", nil
	}
	if stored.Mode == "off" {
		return Config{Mode: "off"}, "disabled", nil
	}
	if stored.Mode == "system" {
		configuration, source, err := m.system()
		configuration.RegistryCAs = stored.RegistryCAs
		return configuration, source, err
	}
	if stored.Mode != "manual" {
		return Config{}, "", errors.New("invalid saved proxy mode; repair networking settings")
	}
	proxies := map[string]*string{"http": &stored.HTTPProxy, "https": &stored.HTTPSProxy, "socks": &stored.SOCKSProxy}
	for name, value := range proxies {
		if *value == "" {
			continue
		}
		proxy, err := parseProxy(*value)
		if err != nil {
			return Config{}, "", err
		}
		if account := stored.CredentialRefs[name]; account != "" {
			secret, err := m.secrets.Get(secretService, account)
			if err != nil {
				return Config{}, "", fmt.Errorf("proxy credentials are unavailable in the native vault; save them again: %w", err)
			}
			if secret == "" {
				return Config{}, "", errors.New("proxy credentials are empty in the native vault; save them again")
			}
			authenticated, err := url.Parse(proxy.Scheme + "://" + secret + "@" + proxy.Host)
			if err != nil {
				return Config{}, "", errors.New("stored proxy credentials are invalid; save them again")
			}
			proxy = authenticated
		}
		*value = proxy.String()
	}
	return stored.Config, "manual", nil
}

func (m *Manager) Snapshot() (Status, error) {
	stored, err := m.load()
	if err != nil {
		return Status{}, err
	}
	status := Status{Config: stored.Config, CredentialsStored: map[string]bool{}}
	for name, account := range stored.CredentialRefs {
		status.CredentialsStored[name] = account != ""
	}
	resolved, source, err := m.resolved()
	if err != nil {
		status.Message = err.Error()
		return status, err
	}
	status.Source = source
	if stored.Mode == "system" {
		status.HTTPProxy = redactURL(resolved.HTTPProxy)
		status.HTTPSProxy = redactURL(resolved.HTTPSProxy)
		status.SOCKSProxy = redactURL(resolved.SOCKSProxy)
		status.NoProxy = resolved.NoProxy
	}
	return status, nil
}

func (m *Manager) ProxyForURL(target *url.URL) (*url.URL, error) {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		host == "porto.local" || strings.HasSuffix(host, ".porto.local") {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil, nil
	}
	configuration, _, err := m.resolved()
	if err != nil {
		return nil, err
	}
	if !strings.Contains(host, ".") && strings.Contains(configuration.NoProxy, "<local>") {
		return nil, nil
	}
	httpProxy, httpsProxy := configuration.HTTPProxy, configuration.HTTPSProxy
	if httpProxy == "" {
		httpProxy = configuration.SOCKSProxy
	}
	if httpsProxy == "" {
		httpsProxy = configuration.SOCKSProxy
	}
	proxy := httpproxy.Config{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, NoProxy: configuration.NoProxy}
	return proxy.ProxyFunc()(target)
}

func (m *Manager) Environment() ([]string, error) {
	configuration, _, err := m.resolved()
	if err != nil {
		return nil, err
	}
	bypass := localBypass
	if configuration.NoProxy != "" {
		bypass += "," + configuration.NoProxy
	}
	values := map[string]string{
		"HTTP_PROXY": configuration.HTTPProxy, "HTTPS_PROXY": configuration.HTTPSProxy,
		"ALL_PROXY": configuration.SOCKSProxy, "NO_PROXY": bypass,
	}
	if values["HTTP_PROXY"] == "" {
		values["HTTP_PROXY"] = configuration.SOCKSProxy
	}
	if values["HTTPS_PROXY"] == "" {
		values["HTTPS_PROXY"] = configuration.SOCKSProxy
	}
	environment := make([]string, 0, len(values)*2)
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		environment = append(environment, name+"="+values[name], strings.ToLower(name)+"="+values[name])
	}
	registryEnvironment, err := m.hostRegistryEnvironment()
	if err != nil {
		return nil, err
	}
	return append(environment, registryEnvironment...), nil
}

func (m *Manager) Transport() (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport does not support isolated proxy and certificate configuration")
	}
	transport := base.Clone()
	transport.Proxy = func(request *http.Request) (*url.URL, error) { return m.ProxyForURL(request.URL) }
	stored, err := m.load()
	if err != nil {
		return nil, err
	}
	if len(stored.RegistryCAs) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, fmt.Errorf("read system certificate trust: %w", err)
		}
		for _, path := range stored.RegistryCAs {
			data, err := readRegistryCA(path)
			if err != nil {
				return nil, err
			}
			if !roots.AppendCertsFromPEM(data) {
				return nil, errors.New("registry CA bundle does not contain certificates")
			}
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	}
	return transport, nil
}

func readRegistryCA(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry CA: %w", err)
	}
	rest, count := data, 0
	for len(bytes.TrimSpace(rest)) > 0 {
		block, remaining := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("registry CA files must contain only public PEM certificates")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !certificate.IsCA || time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
			return nil, errors.New("registry CA is invalid, expired, or not a certificate authority")
		}
		rest, count = remaining, count+1
	}
	if count == 0 {
		return nil, errors.New("registry CA file is empty")
	}
	return data, nil
}

func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".porto-network-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
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

func redactURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return "[invalid proxy URL]"
	}
	parsed.User = nil
	return parsed.String()
}

func Redact(value string) string {
	return credentialURL.ReplaceAllString(value, "$1://[redacted]@")
}

type RegistryTransport struct{}

func (RegistryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	manager, err := Default()
	if err != nil {
		return nil, err
	}
	transport, err := manager.Transport()
	if err != nil {
		return nil, err
	}
	transport.DisableKeepAlives = true
	return transport.RoundTrip(request)
}

func errorsPAC() error {
	return errors.New("automatic PAC/WPAD proxies require a compatible external resolver; configure explicit HTTP/HTTPS/SOCKS proxies and bypass rules in Porto, or explicitly choose proxy off")
}

func environmentProxy() (Config, bool) {
	get := func(name string) string {
		if value := os.Getenv(name); value != "" {
			return value
		}
		return os.Getenv(strings.ToLower(name))
	}
	configuration := Config{Mode: "system", HTTPProxy: get("HTTP_PROXY"), HTTPSProxy: get("HTTPS_PROXY"), SOCKSProxy: get("ALL_PROXY"), NoProxy: get("NO_PROXY")}
	return configuration, configuration.HTTPProxy != "" || configuration.HTTPSProxy != "" || configuration.SOCKSProxy != ""
}
