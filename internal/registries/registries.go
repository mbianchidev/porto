package registries

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	DockerHubProvider = "docker-hub"
	GitHubProvider    = "github"
	GitLabProvider    = "gitlab"
	CustomProvider    = "custom"

	DockerHubServer = "https://index.docker.io/v1/"

	keyringService = "dev.mbianchi.porto.registry"
)

var ErrCredentialNotFound = errors.New("registry credential was not found")

type Credential struct {
	Server   string
	Username string
	Secret   string
}

type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

type secretBackend interface {
	Set(service, account, secret string) error
	Get(service, account string) (string, error)
	Delete(service, account string) error
}

type systemKeyring struct{}

func (systemKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}

func (systemKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}

func (systemKeyring) Delete(service, account string) error {
	return keyring.Delete(service, account)
}

type Vault struct {
	backend secretBackend
}

func NewVault() *Vault {
	return &Vault{backend: systemKeyring{}}
}

func (v *Vault) Set(profileID int64, secret string) error {
	if v == nil || v.backend == nil {
		return errors.New("system credential store is unavailable")
	}
	if profileID <= 0 {
		return errors.New("registry profile ID must be positive")
	}
	if secret == "" {
		return errors.New("registry credential cannot be empty")
	}
	if err := v.backend.Set(keyringService, credentialAccount(profileID), secret); err != nil {
		return fmt.Errorf("save registry credential in system credential store: %w", err)
	}
	return nil
}

func (v *Vault) Get(profileID int64) (string, error) {
	if v == nil || v.backend == nil {
		return "", errors.New("system credential store is unavailable")
	}
	if profileID <= 0 {
		return "", errors.New("registry profile ID must be positive")
	}
	secret, err := v.backend.Get(keyringService, credentialAccount(profileID))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrCredentialNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read registry credential from system credential store: %w", err)
	}
	if secret == "" {
		return "", ErrCredentialNotFound
	}
	return secret, nil
}

func (v *Vault) Delete(profileID int64) error {
	if v == nil || v.backend == nil {
		return errors.New("system credential store is unavailable")
	}
	if profileID <= 0 {
		return errors.New("registry profile ID must be positive")
	}
	err := v.backend.Delete(keyringService, credentialAccount(profileID))
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrCredentialNotFound
	}
	if err != nil {
		return fmt.Errorf("delete registry credential from system credential store: %w", err)
	}
	return nil
}

func credentialAccount(profileID int64) string {
	return "registry-" + strconv.FormatInt(profileID, 10)
}

func NormalizeProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case DockerHubProvider:
		return DockerHubProvider, nil
	case GitHubProvider:
		return GitHubProvider, nil
	case GitLabProvider:
		return GitLabProvider, nil
	case CustomProvider:
		return CustomProvider, nil
	default:
		return "", errors.New("registry provider must be docker-hub, github, gitlab, or custom")
	}
}

func DefaultServer(provider string) string {
	switch provider {
	case DockerHubProvider:
		return DockerHubServer
	case GitHubProvider:
		return "ghcr.io"
	case GitLabProvider:
		return "registry.gitlab.com"
	default:
		return ""
	}
}

func NormalizeServer(server string) (string, error) {
	server = strings.TrimSpace(server)
	if server == "" {
		return "", errors.New("registry server is required")
	}
	if strings.ContainsAny(server, "\x00\r\n\t ") {
		return "", errors.New("registry server cannot contain whitespace or control characters")
	}
	switch strings.ToLower(strings.TrimSuffix(server, "/")) {
	case "docker.io", "index.docker.io", "registry-1.docker.io",
		"https://docker.io", "https://index.docker.io", "https://registry-1.docker.io",
		"https://index.docker.io/v1":
		return DockerHubServer, nil
	}
	parseValue := server
	if !strings.Contains(parseValue, "://") {
		parseValue = "https://" + parseValue
	}
	parsed, err := url.Parse(parseValue)
	if err != nil {
		return "", fmt.Errorf("parse registry server: %w", err)
	}
	if parsed.Scheme != "https" {
		return "", errors.New("registry server must use HTTPS")
	}
	if parsed.User != nil {
		return "", errors.New("registry server cannot contain user information")
	}
	if parsed.Host == "" {
		return "", errors.New("registry server host is required")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("registry server cannot contain a path")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("registry server cannot contain a query or fragment")
	}
	return strings.ToLower(parsed.Host), nil
}

func ServerForImage(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" || strings.HasPrefix(reference, "-") || strings.ContainsAny(reference, "\x00\r\n\t ") {
		return "", errors.New("invalid container image reference")
	}
	first, remainder, hasPath := strings.Cut(reference, "/")
	if !hasPath || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return DockerHubServer, nil
	}
	if remainder == "" {
		return "", errors.New("container image repository is required")
	}
	return NormalizeServer(first)
}

func ValidateImageServer(server, reference string) error {
	normalizedServer, err := NormalizeServer(server)
	if err != nil {
		return err
	}
	imageServer, err := ServerForImage(reference)
	if err != nil {
		return err
	}
	if normalizedServer != imageServer {
		return fmt.Errorf("test image %q belongs to %s, not %s", reference, imageServer, normalizedServer)
	}
	return nil
}

func DockerConfig(credentials []Credential) ([]byte, error) {
	if len(credentials) == 0 {
		return nil, nil
	}
	auths := make(map[string]dockerAuthEntry, len(credentials))
	for _, credential := range credentials {
		server, err := NormalizeServer(credential.Server)
		if err != nil {
			return nil, err
		}
		if credential.Username == "" || credential.Secret == "" {
			return nil, fmt.Errorf("registry credentials for %s are incomplete", server)
		}
		auths[server] = dockerAuthEntry{
			Auth: base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Secret)),
		}
	}
	document, err := json.Marshal(struct {
		Auths map[string]dockerAuthEntry `json:"auths"`
	}{Auths: auths})
	if err != nil {
		return nil, fmt.Errorf("encode registry credentials: %w", err)
	}
	return document, nil
}
