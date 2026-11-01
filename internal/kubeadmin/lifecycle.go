package kubeadmin

import (
	"errors"
	"os"
	"path/filepath"
)

func (m *Manager) ForgetOwnedState() error {
	if m.root == "" || filepath.Dir(m.root) == m.root {
		return errors.New("refusing to remove an unscoped Kubernetes administration directory")
	}
	if err := m.vault.Destroy(); err != nil {
		return err
	}
	return os.RemoveAll(m.root)
}
