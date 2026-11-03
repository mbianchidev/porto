package kubeadmin

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsControllerReadinessWaitsForUpdatedReplicas(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	source := api.objects["apps/deployments/flux-system/source-controller"]
	_ = unstructured.SetNestedField(source.Object, int64(0), "status", "updatedReplicas")
	state, err := manager.GitOpsDiscover(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if state.Ready {
		t.Fatal("old ready controller pods were reported as the pinned rollout")
	}
}

func TestGitOpsControllerRemovalSeesUnrelatedNamespaces(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	api.objects["kustomize.toolkit.fluxcd.io/kustomizations/other/synthetic-unrelated"] = object(t, `{"apiVersion":"kustomize.toolkit.fluxcd.io/v1","kind":"Kustomization","metadata":{"name":"synthetic-unrelated","namespace":"other","uid":"unrelated-uid","resourceVersion":"1"}}`)
	err := mustGitOpsClient(t, manager, scope).gitOpsControllerHasNoApplications(context.Background(), scope)
	if err == nil || !strings.Contains(err.Error(), "application") {
		t.Fatalf("unrelated application did not block controller removal: %v", err)
	}
}
