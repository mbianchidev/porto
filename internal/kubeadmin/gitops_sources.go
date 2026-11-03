package kubeadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
	"k8s.io/apimachinery/pkg/util/validation"
)

type gitOpsCredential struct {
	Scope                   GitOpsScope
	URL, Username, Password string
}

func gitOpsSourceKey(scope GitOpsScope, name string) string {
	data, _ := json.Marshal(struct {
		Scope GitOpsScope
		Name  string
	}{scope, name})
	sum := sha256.Sum256(data)
	return "gitops-source-" + hex.EncodeToString(sum[:16])
}

func (m *Manager) gitOpsSources() ([]GitOpsSource, error) {
	data, err := os.ReadFile(filepath.Join(m.root, "gitops-sources.json"))
	if errors.Is(err, os.ErrNotExist) {
		return []GitOpsSource{}, nil
	}
	if err != nil {
		return nil, &Error{503, "GitOps source registrations could not be read. Check Porto state storage; no empty inventory was substituted."}
	}
	var result []GitOpsSource
	if len(data) > 1024*1024 || json.Unmarshal(data, &result) != nil {
		return nil, &Error{503, "GitOps source registrations are invalid or oversized. Recover local state before editing sources."}
	}
	return result, nil
}

func (m *Manager) GitOpsSources(scope GitOpsScope) ([]GitOpsSource, error) {
	if err := validateGitOpsScope(scope); err != nil {
		return nil, err
	}
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	sources, err := m.gitOpsSources()
	if err != nil {
		return nil, err
	}
	result := []GitOpsSource{}
	for _, source := range sources {
		if source.GitOpsScope == scope {
			result = append(result, source)
		}
	}
	return result, nil
}

func validateGitOpsURL(address string) error {
	value, err := url.Parse(address)
	if len(address) > 2048 || err != nil || value.Scheme != "https" || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.ForceQuery || value.Fragment != "" || strings.ContainsAny(address, "\r\n\x00") {
		return invalid("GitOps sources require a bounded HTTPS URL without embedded credentials, query tokens or fragments. Configure private access through the credential store.")
	}
	return nil
}

func (m *Manager) SaveGitOpsSource(request GitOpsSourceRequest) (GitOpsSource, error) {
	if err := validateGitOpsScope(request.GitOpsScope); err != nil {
		return GitOpsSource{}, err
	}
	if len(validation.IsDNS1123Label(request.Name)) > 0 || request.Kind != "git" && request.Kind != "helm" {
		return GitOpsSource{}, invalid("A GitOps source needs a valid scoped name and Git or Helm source kind.")
	}
	if err := validateGitOpsURL(request.URL); err != nil {
		return GitOpsSource{}, err
	}
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	sources, err := m.gitOpsSources()
	if err != nil {
		return GitOpsSource{}, err
	}
	index := slices.IndexFunc(sources, func(source GitOpsSource) bool {
		return source.GitOpsScope == request.GitOpsScope && source.Name == request.Name
	})
	source := GitOpsSource{GitOpsScope: request.GitOpsScope, Name: request.Name, Kind: request.Kind, URL: request.URL}
	if index >= 0 {
		previous := sources[index]
		if (previous.URL != source.URL || previous.Kind != source.Kind) && request.Password == nil {
			return GitOpsSource{}, conflict("The source URL or kind changed. Explicitly supply scoped credentials or choose public access; credentials are never forwarded to another source.")
		}
		source.CredentialStored = previous.CredentialStored
	}
	account := gitOpsSourceKey(source.GitOpsScope, source.Name)
	if request.Password != nil {
		if len(*request.Password) > 65536 || strings.ContainsAny(*request.Password, "\x00\r\n") || len(request.Username) > 256 || strings.ContainsAny(request.Username, "\x00\r\n") {
			return GitOpsSource{}, invalid("Invalid or oversized scoped GitOps credential.")
		}
		source.CredentialStored = *request.Password != ""
		if source.CredentialStored {
			if request.Username == "" {
				return GitOpsSource{}, invalid("Private HTTPS source access requires an explicit username and password/token.")
			}
			data, err := json.Marshal(gitOpsCredential{Scope: source.GitOpsScope, URL: source.URL, Username: request.Username, Password: *request.Password})
			if err != nil {
				return GitOpsSource{}, err
			}
			if err := m.vault.Set(account, data); err != nil {
				return GitOpsSource{}, &Error{503, "The scoped GitOps credential could not be encrypted. Unlock the system credential store; no plaintext credential was retained."}
			}
		} else if err := m.vault.Delete(account); err != nil {
			return GitOpsSource{}, &Error{503, "The previous GitOps credential could not be removed. Check local encrypted storage before retrying."}
		}
	}
	if index >= 0 {
		sources[index] = source
	} else {
		if len(sources) >= 256 {
			return GitOpsSource{}, conflict("The bounded GitOps source registry is full. Remove unused local registrations explicitly.")
		}
		sources = append(sources, source)
	}
	if err := m.saveGitOpsSources(sources); err != nil {
		return GitOpsSource{}, err
	}
	return source, nil
}

func (m *Manager) saveGitOpsSources(sources []GitOpsSource) error {
	data, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(m.root, "gitops-sources.json"), data, 0o600); err != nil {
		return &Error{503, "GitOps source metadata could not be persisted. Credentials may already have changed; inspect this registration before retrying."}
	}
	return nil
}

func (m *Manager) DeleteGitOpsSource(scope GitOpsScope, name string) error {
	if err := validateGitOpsScope(scope); err != nil {
		return err
	}
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	sources, err := m.gitOpsSources()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(sources, func(source GitOpsSource) bool { return source.GitOpsScope == scope && source.Name == name })
	if index < 0 {
		return &Error{404, "The selected local GitOps source registration does not exist."}
	}
	if err := m.vault.Delete(gitOpsSourceKey(scope, name)); err != nil {
		return &Error{503, "The scoped source credential could not be removed. No cluster resource was deleted."}
	}
	return m.saveGitOpsSources(slices.Delete(sources, index, index+1))
}

func (m *Manager) gitOpsSource(scope GitOpsScope, name string) (GitOpsSource, *gitOpsCredential, string, error) {
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	sources, err := m.gitOpsSources()
	if err != nil {
		return GitOpsSource{}, nil, "", err
	}
	for _, source := range sources {
		if source.GitOpsScope != scope || source.Name != name {
			continue
		}
		var credential *gitOpsCredential
		if source.CredentialStored {
			data, err := m.vault.Get(gitOpsSourceKey(scope, name))
			if err != nil {
				return source, nil, "", &Error{503, "The scoped GitOps credential is unavailable. Unlock or recover the credential store and reconnect this exact source; public or global credentials were not substituted."}
			}
			credential = &gitOpsCredential{}
			if json.Unmarshal(data, credential) != nil || credential.Scope != scope || credential.URL != source.URL || credential.Password == "" || credential.Username == "" {
				return source, nil, "", &Error{503, "The stored GitOps credential does not match this source scope. Reconnect this exact source explicitly."}
			}
		}
		data, err := json.Marshal(struct {
			Source     GitOpsSource
			Credential *gitOpsCredential
		}{source, credential})
		if err != nil {
			return source, nil, "", err
		}
		sum := sha256.Sum256(data)
		return source, credential, hex.EncodeToString(sum[:]), nil
	}
	return GitOpsSource{}, nil, "", &Error{404, "Connect a GitOps source in this exact context/controller/namespace before installing or updating an application."}
}
