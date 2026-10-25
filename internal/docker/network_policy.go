package docker

import "github.com/mbianchidev/porto/internal/networking"

func (m *Manager) SetNetworkPolicy(policy *networking.Manager) {
	m.registryAuthMu.Lock()
	defer m.registryAuthMu.Unlock()
	m.networkPolicy = policy
}

func (m *Manager) NetworkPolicy() *networking.Manager {
	m.registryAuthMu.RLock()
	defer m.registryAuthMu.RUnlock()
	return m.networkPolicy
}
