package kubeadmin

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsPreviewsRejectReadOnlyUnsupportedAndLostSourceCredentials(t *testing.T) {
	for _, scenario := range []string{"read-only", "unsupported-controller", "lost-credential", "mutable-revision"} {
		t.Run(scenario, func(t *testing.T) {
			manager, api, _, scope := gitOpsFixture(t, "flux")
			request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
			switch scenario {
			case "read-only":
				api.denied["create/kustomizations"] = true
			case "unsupported-controller":
				_ = unstructured.SetNestedSlice(api.objects["apps/deployments/flux-system/source-controller"].Object, []any{map[string]any{"name": "manager", "image": "ghcr.io/fluxcd/source-controller:unsupported"}}, "spec", "template", "spec", "containers")
			case "lost-credential":
				password := "synthetic-private-token"
				if _, err := manager.SaveGitOpsSource(GitOpsSourceRequest{GitOpsScope: scope, Name: request.Source, Kind: "git", URL: "https://git.example.invalid/synthetic.git", Username: "synthetic", Password: &password}); err != nil {
					t.Fatal(err)
				}
				if err := manager.vault.Delete(gitOpsSourceKey(scope, request.Source)); err != nil {
					t.Fatal(err)
				}
			case "mutable-revision":
				request.Revision = "main"
			}
			if _, err := manager.PreviewGitOps(context.Background(), request); err == nil {
				t.Fatal("unsafe preview was accepted")
			}
			if api.applyCount != 0 {
				t.Fatal("preview rejection mutated native resources")
			}
		})
	}
}

func TestGitOpsActivationRechecksCredentialsSchemasAndResourceIdentity(t *testing.T) {
	for _, scenario := range []string{"lost-credential", "changed-schema", "appeared-resource", "revoked-delegation"} {
		t.Run(scenario, func(t *testing.T) {
			manager, api, ops, scope := gitOpsFixture(t, "flux")
			request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
			if scenario == "lost-credential" {
				password := "synthetic-private-token"
				if _, err := manager.SaveGitOpsSource(GitOpsSourceRequest{GitOpsScope: scope, Name: request.Source, Kind: "git", URL: "https://git.example.invalid/synthetic.git", Username: "synthetic", Password: &password}); err != nil {
					t.Fatal(err)
				}
			}
			preview, err := manager.PreviewGitOps(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "revoked-delegation":
				api.denied["impersonate/serviceaccounts"] = true
			case "lost-credential":
				if err := manager.vault.Delete(gitOpsSourceKey(scope, request.Source)); err != nil {
					t.Fatal(err)
				}
			case "changed-schema":
				crd := api.objects["apiextensions.k8s.io/customresourcedefinitions//kustomizations.kustomize.toolkit.fluxcd.io"]
				versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
				version := versions[0].(map[string]any)
				schema, _, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
				schema["description"] = "synthetic reviewed schema changed"
				_ = unstructured.SetNestedMap(version, schema, "schema", "openAPIV3Schema")
				_ = unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions")
			case "appeared-resource":
				api.objects["source.toolkit.fluxcd.io/gitrepositories/default/synthetic-app-source"] = object(t, `{"apiVersion":"source.toolkit.fluxcd.io/v1","kind":"GitRepository","metadata":{"name":"synthetic-app-source","namespace":"default","uid":"unrelated-uid","resourceVersion":"1"}}`)
			}
			op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
			if err != nil {
				t.Fatal(err)
			}
			result := waitAdmin(t, ops, op.ID)
			if result.Status != "failed" || api.applyCount != 0 {
				t.Fatalf("activation did not fail before mutation: %+v count=%d", result, api.applyCount)
			}
			if strings.Contains(result.Error, "synthetic-private-token") {
				t.Fatal("operation history exposed a credential")
			}
		})
	}
}

func TestGitOpsInterruptedInstallRetainsPartialStateWithoutPretendingRollback(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
	preview, err := manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	api.failSecondApply = true
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result := waitAdmin(t, ops, op.ID)
	if result.Status != "partial" || api.objects["source.toolkit.fluxcd.io/gitrepositories/default/synthetic-app-source"] == nil || api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"] != nil {
		t.Fatalf("partial installation was hidden or rolled back: %+v", result)
	}
}

func TestGitOpsStaleHealthyRevisionCannotHideUnavailableSource(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "flux", "Kustomization", "git")
	api.mu.Lock()
	root := api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"]
	_ = unstructured.SetNestedField(root.Object, false, "spec", "suspend")
	root.Object["status"] = map[string]any{"observedGeneration": root.GetGeneration(), "lastAppliedRevision": "main@sha1:" + strings.Repeat("a", 40), "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": root.GetGeneration()}}}
	api.mu.Unlock()
	current, err := manager.GitOpsApplication(context.Background(), scope, "Kustomization", "synthetic-app")
	if err != nil {
		t.Fatal(err)
	}
	if current.Health == "healthy" || current.Phase == "ready" {
		t.Fatalf("stale revision hid missing source readiness: %+v", current)
	}
}
