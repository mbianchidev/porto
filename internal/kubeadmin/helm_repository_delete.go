package kubeadmin

import (
	"encoding/json"
	"path/filepath"
	"slices"

	"github.com/mbianchidev/porto/internal/config"
)

func (m *Manager) DeleteHelmRepository(name string) error {
	if !helmName.MatchString(name) {
		return invalid("Select a valid Helm repository name.")
	}
	m.repoMu.Lock()
	defer m.repoMu.Unlock()
	repositories, err := m.HelmRepositories()
	if err != nil {
		return err
	}
	index := slices.IndexFunc(repositories, func(value HelmRepository) bool { return value.Name == name })
	if index < 0 {
		return &Error{404, "The selected local Helm repository is not configured."}
	}
	repositories = append(repositories[:index], repositories[index+1:]...)
	data, err := json.Marshal(repositories)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(filepath.Join(m.root, "helm-repositories.json"), data, 0o600); err != nil {
		return err
	}
	return m.vault.Delete("helm-repo-" + name)
}
