package kubernetes

import (
	"errors"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func (m *Manager) AdminConfig(contextName string) (*rest.Config, clientcmdapi.Config, error) {
	if strings.TrimSpace(contextName) == "" {
		return nil, clientcmdapi.Config{}, errors.New("an explicit Porto-managed Kubernetes context is required")
	}
	path := m.kubeconfigForContext(contextName)
	if path == "" {
		return nil, clientcmdapi.Config{}, errors.New("the selected managed kubeconfig is unavailable; an unrelated global context will not be used")
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}
	configuration := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName})
	raw, err := configuration.RawConfig()
	if err != nil {
		return nil, raw, errors.New("the selected managed kubeconfig could not be loaded")
	}
	if raw.Contexts[contextName] == nil {
		return nil, raw, errors.New("the selected context is not present in its managed kubeconfig")
	}
	raw.CurrentContext = contextName
	if err := clientcmdapi.MinifyConfig(&raw); err != nil {
		return nil, raw, errors.New("the selected Kubernetes identity is incomplete")
	}
	resolved, err := configuration.ClientConfig()
	if err != nil {
		return nil, raw, errors.New("the selected Kubernetes identity could not be configured")
	}
	resolved.Timeout = 30 * time.Second
	resolved.QPS, resolved.Burst = 10, 20
	resolved.UserAgent = "Porto Kubernetes administration"
	return resolved, raw, nil
}
