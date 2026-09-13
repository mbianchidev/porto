package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mbianchidev/porto/internal/registries"
)

const (
	dockerHubRegistry          = registries.DockerHubServer
	maxRegistryAuthHeaderBytes = 64 * 1024
)

type RegistryAuth struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
}

type registryAuthEntry struct {
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

func decodeRegistryAuthHeader(value string) (*RegistryAuth, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if len(value) > maxRegistryAuthHeaderBytes {
		return nil, errors.New("invalid registry auth header: value is too large")
	}
	var decoded []byte
	var decodeErr error
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		decoded, decodeErr = encoding.DecodeString(value)
		if decodeErr == nil {
			break
		}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("invalid registry auth header: %w", decodeErr)
	}
	var auth RegistryAuth
	if err := json.Unmarshal(decoded, &auth); err != nil {
		return nil, fmt.Errorf("invalid registry auth header: %w", err)
	}
	if !auth.hasCredentials() {
		return nil, nil
	}
	return &auth, nil
}

func registryDockerConfig(reference string, auth *RegistryAuth) ([]byte, bool, error) {
	if auth == nil || !auth.hasCredentials() {
		return nil, false, nil
	}
	if auth.RegistryToken != "" {
		return nil, false, fmt.Errorf("%w: registry token authentication", ErrUnsupported)
	}
	var registry string
	var err error
	if strings.TrimSpace(auth.ServerAddress) == "" {
		registry, err = registries.ServerForImage(reference)
	} else {
		registry, err = registries.NormalizeServer(auth.ServerAddress)
	}
	if err != nil {
		return nil, false, fmt.Errorf("invalid registry address: %w", err)
	}
	entry := registryAuthEntry{
		Auth:          auth.Auth,
		IdentityToken: auth.IdentityToken,
	}
	if entry.Auth == "" && (auth.Username != "" || auth.Password != "") {
		entry.Auth = base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
	}
	document, err := json.Marshal(struct {
		Auths map[string]registryAuthEntry `json:"auths"`
	}{
		Auths: map[string]registryAuthEntry{registry: entry},
	})
	if err != nil {
		return nil, false, fmt.Errorf("encode registry auth: %w", err)
	}
	return document, true, nil
}

func (auth RegistryAuth) hasCredentials() bool {
	return auth.Auth != "" ||
		auth.IdentityToken != "" ||
		auth.RegistryToken != "" ||
		auth.Username != "" ||
		auth.Password != ""
}
