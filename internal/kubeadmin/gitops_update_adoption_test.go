package kubeadmin

import (
	"context"
	"testing"
)

func TestGitOpsConfigurationUpdateRetainsStagedAdoptionRecoveryInventory(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
	target := Target{Context: scope.Context, Namespace: scope.Namespace, Version: "v1", Resource: "configmaps", Name: "synthetic-existing", UID: "existing-uid", ResourceVersion: "1"}
	api.objects["/configmaps/default/synthetic-existing"] = object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic-existing","namespace":"default","uid":"existing-uid","resourceVersion":"1"}}`)
	request.Action, request.Inventory = "adopt", []Target{target}
	preview, err := manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	current, err := manager.GitOpsApplication(context.Background(), scope, request.Kind, request.Name)
	if err != nil {
		t.Fatal(err)
	}
	request.Action, request.UID, request.ResourceVersion = "update", current.Target.UID, current.Target.ResourceVersion
	preview, err = manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	root := api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"]
	inventory, err := gitOpsAdoptionTargets(root)
	if err != nil || len(inventory) != 1 || root.GetAnnotations()["porto.dev/gitops-adoption"] != "staged" {
		t.Fatalf("configuration update lost staged ownership recovery: %+v %v", inventory, err)
	}
}
