package kubeadmin

import (
	"context"
	"strings"
	"testing"
)

func TestGitOpsOwnershipBlocksDirectManifestWriters(t *testing.T) {
	for _, owner := range []string{
		`"labels":{"kustomize.toolkit.fluxcd.io/name":"synthetic-app","kustomize.toolkit.fluxcd.io/namespace":"default"}`,
		`"labels":{"helm.toolkit.fluxcd.io/name":"synthetic-app","helm.toolkit.fluxcd.io/namespace":"default"}`,
		`"annotations":{"argocd.argoproj.io/tracking-id":"synthetic-app:/ConfigMap:default/synthetic"}`,
		`"annotations":{"porto.dev/gitops-handoff":"synthetic-owner"}`,
	} {
		t.Run(owner, func(t *testing.T) {
			manager, api, _ := adminFixture(t)
			api.objects["/configmaps/default/synthetic"] = object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic","namespace":"default","uid":"synthetic-uid","resourceVersion":"1",`+owner+`},"data":{"setting":"original"}}`)
			_, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic"},"data":{"setting":"changed"}}`})
			if err == nil || !strings.Contains(err.Error(), "GitOps") {
				t.Fatalf("direct writer did not reject GitOps ownership: %v", err)
			}
		})
	}
}
