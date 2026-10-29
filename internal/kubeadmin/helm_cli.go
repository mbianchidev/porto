package kubeadmin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/runtimes"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func validateHelmScope(scope HelmScope) error {
	if scope.Context == "" || !safeSegment.MatchString(scope.Namespace) || strings.Contains(scope.Namespace, ":") {
		return invalid("Select an explicit managed Kubernetes context and namespace for Helm.")
	}
	return nil
}

func (m *Manager) helm(ctx context.Context, scope HelmScope, values []byte, args ...string) ([]byte, error) {
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(m.root, ".helm-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	probe, err := m.runner.Run(ctx, runtimes.Command{Name: "helm", Args: []string{"version", "--template", "{{.Version}}"}})
	if err != nil {
		return nil, &Error{503, "Helm is unavailable. Desktop packages include verified Helm 4.3.1; source installations need that version on the daemon PATH."}
	}
	version := strings.TrimPrefix(strings.TrimSpace(string(probe.Stdout)), "v")
	if version != HelmVersion {
		return nil, &Error{503, "Helm tool version differs from the verified 4.3.1 contract. Install the matching tool or update Porto's reviewed toolchain."}
	}
	environment := []string{"HELM_CONFIG_HOME=" + filepath.Join(work, "config"), "HELM_DATA_HOME=" + filepath.Join(work, "data"), "HELM_CACHE_HOME=" + filepath.Join(m.root, "helm-cache"), "HELM_REGISTRY_CONFIG=" + filepath.Join(work, "registry.json"), "HELM_DRIVER=secret", "HELM_DEBUG=false"}
	options := []string{}
	if scope.Context != "" {
		if err := validateHelmScope(scope); err != nil {
			return nil, err
		}
		_, raw, err := m.configs.AdminConfig(scope.Context)
		if err != nil {
			return nil, &Error{503, "The selected managed Helm kubeconfig is unavailable; no global identity was used."}
		}
		if err := clientcmdapi.FlattenConfig(&raw); err != nil {
			return nil, &Error{503, "The selected managed kubeconfig could not be flattened privately for Helm."}
		}
		data, err := clientcmd.Write(raw)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(work, "kubeconfig.yaml")
		if err := config.WriteFileAtomic(path, data, 0o600); err != nil {
			return nil, err
		}
		options = append(options, "--kubeconfig", path, "--kube-context", scope.Context, "--namespace", scope.Namespace)
		environment = append(environment, "KUBECONFIG="+path, "HELM_KUBECONTEXT="+scope.Context, "HELM_NAMESPACE="+scope.Namespace)
	}
	if len(args) > 0 && (args[0] == "repo" || args[0] == "pull") {
		path, err := m.helmRepositoryConfig(work)
		if err != nil {
			return nil, err
		}
		options = append(options, "--repository-config", path, "--repository-cache", filepath.Join(m.root, "helm-cache", "repository"))
	}
	if values != nil {
		if !json.Valid(values) {
			return nil, invalid("Helm values must be valid JSON; sensitive values are not accepted as command arguments.")
		}
		path := filepath.Join(work, "values.json")
		if err := config.WriteFileAtomic(path, values, 0o600); err != nil {
			return nil, err
		}
		options = append(options, "--values", path)
	}
	policy, err := networking.Default()
	if err != nil {
		return nil, err
	}
	network, err := policy.Environment()
	if err != nil {
		return nil, &Error{503, "Helm networking/proxy credentials are unavailable. Check Porto networking settings."}
	}
	environment = append(network, environment...)
	arguments := append(append([]string{}, args...), options...)
	commandContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	output, err := m.runner.Run(commandContext, runtimes.Command{Name: "helm", Args: arguments, Env: environment, Dir: work})
	if err != nil {
		if commandContext.Err() != nil {
			return nil, commandContext.Err()
		}
		message := strings.ToLower(string(output.Stderr))
		if strings.Contains(message, "release: not found") {
			return nil, &Error{404, "The selected Helm release does not exist in this namespace."}
		}
		if strings.Contains(message, "forbidden") || strings.Contains(message, "unauthorized") {
			return nil, &Error{403, "The selected Helm identity or repository access was denied. No alternate identity or insecure transport was used."}
		}
		return nil, &Error{502, "Helm did not complete. Check the selected release status/history, chart version, source access, RBAC and hook conditions. Raw tool output was withheld because it can contain Secret values."}
	}
	return output.Stdout, nil
}

func (m *Manager) helmRepositoryConfig(work string) (string, error) {
	repositories, err := m.HelmRepositories()
	if err != nil {
		return "", err
	}
	type entry struct {
		Name               string `yaml:"name"`
		URL                string `yaml:"url"`
		Username           string `yaml:"username,omitempty"`
		Password           string `yaml:"password,omitempty"`
		PassCredentialsAll bool   `yaml:"pass_credentials_all"`
		Insecure           bool   `yaml:"insecure_skip_tls_verify"`
	}
	entries := []entry{}
	for _, repository := range repositories {
		current := entry{Name: repository.Name, URL: repository.URL}
		if repository.CredentialStored {
			data, err := m.vault.Get("helm-repo-" + repository.Name)
			if err != nil {
				return "", &Error{503, "A selected Helm repository credential is unavailable. Unlock the system credential store; no global repository identity was substituted."}
			}
			var credential struct{ URL, Username, Password string }
			if json.Unmarshal(data, &credential) != nil {
				return "", &Error{503, "Stored Helm repository credentials are invalid."}
			}
			if credential.URL != repository.URL {
				return "", &Error{503, "Stored Helm credentials are scoped to a different source URL. Update this repository's credentials explicitly."}
			}
			current.Username, current.Password = credential.Username, credential.Password
		}
		entries = append(entries, current)
	}
	data, err := yaml.Marshal(struct {
		APIVersion   string  `yaml:"apiVersion"`
		Repositories []entry `yaml:"repositories"`
	}{"v1", entries})
	if err != nil {
		return "", err
	}
	path := filepath.Join(work, "repositories.yaml")
	return path, config.WriteFileAtomic(path, data, 0o600)
}

func missingHelmRelease(err error) bool {
	var known *Error
	return errors.As(err, &known) && known.Code == 404
}
