package environments

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func ImportKubeconfig(document, contextName string) ([]byte, string, string, string, error) {
	if len(document) == 0 || len(document) > 1<<20 || contextName == "" {
		return nil, "", "", "", problem(400, "kubeconfig", "Explicit kubeconfig content (up to 1 MiB) and a context name are required. Global kubeconfigs are never imported implicitly.")
	}
	raw, err := clientcmd.Load([]byte(document))
	if err != nil {
		return nil, "", "", "", problem(400, "kubeconfig", "The supplied kubeconfig is invalid.")
	}
	if raw.Contexts[contextName] == nil {
		return nil, "", "", "", problem(400, "context", "The requested context is absent from the supplied kubeconfig.")
	}
	raw.CurrentContext = contextName
	if err := clientcmdapi.MinifyConfig(raw); err != nil {
		return nil, "", "", "", problem(400, "kubeconfig", "The selected kubeconfig identity is incomplete.")
	}
	context := raw.Contexts[contextName]
	cluster, auth := raw.Clusters[context.Cluster], raw.AuthInfos[context.AuthInfo]
	if cluster == nil || auth == nil {
		return nil, "", "", "", problem(400, "kubeconfig", "An explicit cluster and identity are required.")
	}
	server, err := endpoint(cluster.Server)
	if err != nil {
		return nil, "", "", "", problem(400, "kubernetes_tls", "Imported Kubernetes connections require an HTTPS origin.")
	}
	if cluster.InsecureSkipTLSVerify || cluster.CertificateAuthority != "" || cluster.ProxyURL != "" || auth.Exec != nil || auth.AuthProvider != nil ||
		auth.TokenFile != "" || auth.ClientCertificate != "" || auth.ClientKey != "" || auth.Username != "" || auth.Password != "" ||
		auth.Impersonate != "" || len(auth.ImpersonateGroups) > 0 || len(auth.ImpersonateUserExtra) > 0 {
		return nil, "", "", "", problem(400, "kubeconfig_authority", "Use inline CA and bearer token or client-certificate data. Insecure TLS, file references, proxy URLs, executable/auth-provider plugins, basic auth and impersonation are not imported.")
	}
	if auth.Token == "" && (len(auth.ClientCertificateData) == 0 || len(auth.ClientKeyData) == 0) {
		return nil, "", "", "", problem(400, "kubernetes_identity", "Provide an explicit bearer token or complete inline client certificate/key.")
	}
	if len(auth.ClientCertificateData) > 0 {
		if _, err := tls.X509KeyPair(auth.ClientCertificateData, auth.ClientKeyData); err != nil {
			return nil, "", "", "", problem(400, "kubernetes_identity", "The inline Kubernetes client certificate/key is invalid.")
		}
	}
	data, err := clientcmd.Write(*raw)
	if err != nil {
		return nil, "", "", "", err
	}
	return data, server.String(), context.AuthInfo, contextName, nil
}

type KubernetesConfig struct {
	Manager       *Manager
	EnvironmentID string
}

func (p KubernetesConfig) AdminConfig(contextName string) (*rest.Config, clientcmdapi.Config, error) {
	profile, _, _, err := p.Manager.target(p.EnvironmentID)
	if err != nil {
		return nil, clientcmdapi.Config{}, err
	}
	if profile.Backend != "kubernetes" || profile.Context != contextName {
		return nil, clientcmdapi.Config{}, problem(403, "wrong_context", "The context must exactly match the explicitly imported environment; no global or owned-cluster context is substituted.")
	}
	value, err := p.Manager.getSecret(profile.ID)
	if err != nil {
		return nil, clientcmdapi.Config{}, err
	}
	raw, err := clientcmd.Load([]byte(value.Kubeconfig))
	if err != nil {
		return nil, clientcmdapi.Config{}, fmt.Errorf("read enrolled Kubernetes identity: %w", err)
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, contextName, &clientcmd.ConfigOverrides{}, nil).ClientConfig()
	if err != nil {
		return nil, *raw, err
	}
	config.Timeout, config.QPS, config.Burst = 30*time.Second, 10, 20
	config.UserAgent = "Porto imported environment " + profile.ID
	config.Proxy = nil
	config.WrapTransport = func(base http.RoundTripper) http.RoundTripper { return noRedirectTransport{base} }
	if !strings.HasPrefix(config.Host, "https://") || config.Insecure {
		return nil, *raw, problem(403, "kubernetes_tls", "Imported Kubernetes identity no longer has verified HTTPS configuration.")
	}
	return config, *raw, nil
}

type noRedirectTransport struct{ base http.RoundTripper }

func (t noRedirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		response.Body.Close()
		return nil, errors.New("redirects are not authorized for an explicitly selected Kubernetes environment")
	}
	return response, nil
}

func (m *Manager) TargetContext(id string) (context.Context, error) {
	_, target, _, err := m.target(id)
	if err != nil {
		return nil, err
	}
	return target.ctx, nil
}
