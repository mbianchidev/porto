package kubeadmin

import (
	"context"
	"testing"
)

func TestGitOpsHelmHandoffVerificationRequiresTheNativeReleaseOwner(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	manager.runner = &helmRunner{t: t, api: api, revision: 1, status: "deployed"}
	if _, err := manager.helmRelease(context.Background(), HelmScope{Context: scope.Context, Namespace: scope.Namespace}, "synthetic-release"); err != nil {
		t.Fatal(err)
	}
	request := gitOpsInstallRequest(t, manager, api, scope, "HelmRelease", "helm")
	request.Name, request.Action, request.HelmRelease = "synthetic-release", "adopt", "synthetic-release"
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
	api.mu.Lock()
	root := api.objects["helm.toolkit.fluxcd.io/helmreleases/default/synthetic-release"]
	root.Object["status"] = map[string]any{"observedGeneration": root.GetGeneration(), "lastAttemptedRevision": request.Revision, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": root.GetGeneration()}}, "history": []any{map[string]any{"name": "synthetic-release", "namespace": "default", "version": int64(2), "chartVersion": request.Revision, "status": "deployed"}}}
	source := api.objects["source.toolkit.fluxcd.io/helmrepositories/default/synthetic-release-source"]
	source.Object["status"] = map[string]any{"observedGeneration": source.GetGeneration(), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": source.GetGeneration()}}}
	api.objects["/secrets/default/sh.helm.release.v1.synthetic-release.v2"] = object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"sh.helm.release.v1.synthetic-release.v2","namespace":"default","uid":"helm-2","resourceVersion":"2","labels":{"helm.toolkit.fluxcd.io/name":"synthetic-release","helm.toolkit.fluxcd.io/namespace":"default"}}}`)
	api.mu.Unlock()
	current, err := manager.GitOpsApplication(context.Background(), scope, "HelmRelease", request.Name)
	if err != nil {
		t.Fatal(err)
	}
	verify := GitOpsRequest{GitOpsScope: scope, Action: "verify-adoption", Kind: current.Kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion}
	preview, err = manager.PreviewGitOps(context.Background(), verify)
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
	current, err = manager.GitOpsApplication(context.Background(), scope, current.Kind, current.Name)
	if err != nil || current.Adoption != "verified" {
		t.Fatalf("native Helm handoff was not verified: %+v %v", current, err)
	}
}
