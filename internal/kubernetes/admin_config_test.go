package kubernetes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbianchidev/porto/internal/config"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestAdminConfigPinsManagedContextWithoutGlobalFallback(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, config.KubernetesClusterFileToken("synthetic")+".yaml")
	raw := clientcmdapi.Config{CurrentContext: "unrelated", Clusters: map[string]*clientcmdapi.Cluster{"selected": {Server: "https://selected.example.invalid"}, "other": {Server: "https://unrelated.example.invalid"}}, Contexts: map[string]*clientcmdapi.Context{"porto-synthetic": {Cluster: "selected", AuthInfo: "synthetic"}, "unrelated": {Cluster: "other", AuthInfo: "synthetic"}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"synthetic": {Token: "synthetic-token"}}}
	data, err := clientcmd.Write(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	manager := NewWithKubeconfigRoot(nil, root)
	selected, minified, err := manager.AdminConfig("porto-synthetic")
	if err != nil || selected.Host != "https://selected.example.invalid" || minified.CurrentContext != "porto-synthetic" || len(minified.Contexts) != 1 {
		t.Fatalf("administration inherited another context: %+v %v", selected, err)
	}
	for _, name := range []string{"", "porto-missing", "unrelated"} {
		if _, _, err := manager.AdminConfig(name); err == nil {
			t.Fatalf("global fallback accepted for %q", name)
		}
	}
}
