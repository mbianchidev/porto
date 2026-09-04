package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

const dockerHubRegistry = "https://index.docker.io/v1/"

var dockerCredentialHelperPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type dockerConfigDocument struct {
	Auths       map[string]dockerStoredAuth `json:"auths"`
	CredHelpers map[string]string           `json:"credHelpers"`
	CredsStore  string                      `json:"credsStore"`
}

type dockerStoredAuth struct {
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
}

type dockerScopedConfig struct {
	Auths map[string]dockerStoredAuth `json:"auths"`
}

type dockerHelperCredential struct {
	Username string `json:"Username"`
	Secret   string `json:"Secret"`
}

func (p *ClusterProvisioner) runKindCreate(ctx context.Context, args []string) ([]byte, error) {
	environment, cleanup, err := p.kindCreateDockerEnv(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return p.runner.Run(ctx, runtimes.Command{Name: "kind", Args: args, Env: environment})
}

func (p *ClusterProvisioner) kindCreateDockerEnv(ctx context.Context) ([]string, func(), error) {
	environment := p.portoDockerEnv()
	auth, found := p.kindRegistryAuth(ctx, dockerHubRegistry)
	if !found {
		return environment, func() {}, nil
	}
	configDir, err := os.MkdirTemp("", "porto-kind-docker-*")
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary kind Docker config: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(configDir); err != nil {
			log.Printf("remove temporary kind Docker config: %v", err)
		}
	}
	document, err := json.Marshal(dockerScopedConfig{
		Auths: map[string]dockerStoredAuth{dockerHubRegistry: auth},
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("encode temporary kind Docker config: %w", err)
	}
	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, document, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write temporary kind Docker config: %w", err)
	}
	if err := os.Chmod(configPath, 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("protect temporary kind Docker config: %w", err)
	}
	return replaceEnvironmentVariable(environment, "DOCKER_CONFIG", configDir), cleanup, nil
}

func (p *ClusterProvisioner) kindRegistryAuth(
	ctx context.Context,
	registry string,
) (dockerStoredAuth, bool) {
	configPath, err := userDockerConfigPath()
	if err != nil {
		log.Printf("resolve user Docker config for kind authentication: %v; using anonymous pull", err)
		return dockerStoredAuth{}, false
	}
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return dockerStoredAuth{}, false
	}
	if err != nil {
		log.Printf("read user Docker config for kind authentication: %v; using anonymous pull", err)
		return dockerStoredAuth{}, false
	}
	var document dockerConfigDocument
	if err := json.Unmarshal(data, &document); err != nil {
		log.Printf("decode user Docker config for kind authentication: %v; using anonymous pull", err)
		return dockerStoredAuth{}, false
	}
	helper, helperRegistry := configuredCredentialHelper(document, registry)
	if helper != "" {
		auth, found, err := p.authFromCredentialHelper(ctx, helper, helperRegistry)
		if err != nil {
			log.Printf("resolve kind registry credentials with Docker helper %q: %v; using stored or anonymous credentials", helper, err)
		} else if found {
			return auth, true
		}
	}
	for _, alias := range dockerRegistryAliases(registry) {
		auth, ok := document.Auths[alias]
		if ok {
			if scoped, found := auth.scoped(); found {
				return scoped, true
			}
		}
	}
	return dockerStoredAuth{}, false
}

func (p *ClusterProvisioner) authFromCredentialHelper(
	ctx context.Context,
	helper string,
	registry string,
) (dockerStoredAuth, bool, error) {
	if !dockerCredentialHelperPattern.MatchString(helper) {
		return dockerStoredAuth{}, false, fmt.Errorf("invalid Docker credential helper name %q", helper)
	}
	helperContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := p.runner.Run(helperContext, runtimes.Command{
		Name:  "docker-credential-" + helper,
		Args:  []string{"get"},
		Stdin: []byte(registry + "\n"),
	})
	if err != nil {
		if dockerCredentialsMissing(output) {
			return dockerStoredAuth{}, false, nil
		}
		return dockerStoredAuth{}, false, fmt.Errorf("credential helper failed: %w", err)
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		return dockerStoredAuth{}, false, nil
	}
	var credential dockerHelperCredential
	if err := json.Unmarshal(output, &credential); err != nil {
		return dockerStoredAuth{}, false, fmt.Errorf("decode credential helper response: %w", err)
	}
	if credential.Username == "" && credential.Secret == "" {
		return dockerStoredAuth{}, false, nil
	}
	if credential.Username == "<token>" {
		return dockerStoredAuth{IdentityToken: credential.Secret}, true, nil
	}
	return dockerStoredAuth{
		Auth: base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Secret)),
	}, true, nil
}

func (auth dockerStoredAuth) scoped() (dockerStoredAuth, bool) {
	scoped := dockerStoredAuth{
		Auth:          auth.Auth,
		IdentityToken: auth.IdentityToken,
	}
	if scoped.Auth == "" && (auth.Username != "" || auth.Password != "") {
		scoped.Auth = base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
	}
	if scoped.Auth == "" && scoped.IdentityToken == "" {
		return dockerStoredAuth{}, false
	}
	return scoped, true
}

func userDockerConfigPath() (string, error) {
	configDir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG"))
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".docker")
	}
	return filepath.Join(configDir, "config.json"), nil
}

func configuredCredentialHelper(document dockerConfigDocument, registry string) (string, string) {
	for _, alias := range dockerRegistryAliases(registry) {
		if helper := strings.TrimSpace(document.CredHelpers[alias]); helper != "" {
			return helper, alias
		}
	}
	return strings.TrimSpace(document.CredsStore), registry
}

func dockerRegistryAliases(registry string) []string {
	if registry == dockerHubRegistry {
		return []string{
			dockerHubRegistry,
			"index.docker.io",
			"registry-1.docker.io",
			"docker.io",
		}
	}
	return []string{registry}
}

func dockerCredentialsMissing(output []byte) bool {
	message := strings.ToLower(strings.TrimSpace(string(output)))
	return strings.Contains(message, "credentials not found") ||
		strings.Contains(message, "no credentials") ||
		strings.Contains(message, "not found in native keychain")
}

func replaceEnvironmentVariable(environment []string, name, value string) []string {
	prefix := name + "="
	replaced := false
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				result = append(result, prefix+value)
				replaced = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !replaced {
		result = append(result, prefix+value)
	}
	return result
}
