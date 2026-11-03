package kubeadmin

import (
	"context"
	"strings"
	"testing"
)

func TestGitOpsHelmOwnershipBlocksDirectLifecycleEvenWhenSuspended(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	manager.runner = &helmRunner{t: t, api: api, revision: 1, status: "deployed"}
	api.objects["helm.toolkit.fluxcd.io/helmreleases/default/synthetic-release"] = object(t, `{"apiVersion":"helm.toolkit.fluxcd.io/v2","kind":"HelmRelease","metadata":{"name":"synthetic-release","namespace":"default","uid":"synthetic-hr","resourceVersion":"1"},"spec":{"suspend":true,"releaseName":"synthetic-release","targetNamespace":"default","storageNamespace":"default"}}`)
	_, err := manager.PreviewHelm(context.Background(), HelmRequest{HelmScope: HelmScope{Context: scope.Context, Namespace: scope.Namespace}, Release: "synthetic-release", Action: "uninstall", SecretConsent: true})
	if err == nil || !strings.Contains(err.Error(), "GitOps") {
		t.Fatalf("direct Helm bypassed suspended controller ownership: %v", err)
	}
}

func TestGitOpsHelmAdoptionUsesOneExistingReleaseAndStagesItsStore(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	runner := &helmRunner{t: t, api: api, revision: 1, status: "deployed"}
	manager.runner = runner
	_, err := manager.helmRelease(context.Background(), HelmScope{Context: scope.Context, Namespace: scope.Namespace}, "synthetic-release")
	if err != nil {
		t.Fatal(err)
	}
	request := gitOpsInstallRequest(t, manager, api, scope, "HelmRelease", "helm")
	request.Name = "synthetic-release"
	if _, err := manager.PreviewGitOps(context.Background(), request); err == nil {
		t.Fatal("existing direct Helm release was silently claimed without a handoff")
	}
	request.Action, request.HelmRelease = "adopt", "synthetic-release"
	preview, err := manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatalf("Helm handoff: %+v", result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	secret := api.objects["/secrets/default/sh.helm.release.v1.synthetic-release.v1"]
	if secret.GetAnnotations()[gitOpsHandoff] == "" {
		t.Fatal("native Helm release store was not locked during handoff")
	}
	root := api.objects["helm.toolkit.fluxcd.io/helmreleases/default/synthetic-release"]
	if gitOpsString(root, "spec", "releaseName") != "synthetic-release" || !gitOpsBool(root, "spec", "suspend") {
		t.Fatal("Helm handoff created another release or enabled reconciliation")
	}
	if runner.mutations != 0 {
		t.Fatal("staging handoff ran native Helm mutations")
	}
}
