package kubeadmin

import (
	"context"
	"testing"
)

func TestGitOpsDependenciesRejectIndirectCyclesBeforeMutation(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
	request.Dependencies = []string{"synthetic-dependency"}
	api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-dependency"] = object(t, `{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"Kustomization","metadata":{"name":"synthetic-dependency","namespace":"default","uid":"dependency-uid","resourceVersion":"1"},"spec":{"dependsOn":[{"name":"synthetic-app"}]}}`)
	if _, err := manager.PreviewGitOps(context.Background(), request); err == nil {
		t.Fatal("an indirect reconciliation dependency cycle was accepted")
	}
	if api.applyCount != 0 {
		t.Fatal("dependency validation mutated resources")
	}
}
