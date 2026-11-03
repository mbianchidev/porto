package kubeadmin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func gitOpsInstallRequest(t *testing.T, manager *Manager, api *fakeAPI, scope GitOpsScope, kind, sourceKind string) GitOpsRequest {
	t.Helper()
	sourceURL := "https://git.example.invalid/synthetic.git"
	revision := strings.Repeat("a", 40)
	if sourceKind == "helm" {
		sourceURL, revision = "https://charts.example.invalid/", "1.2.3"
	}
	if _, err := manager.SaveGitOpsSource(GitOpsSourceRequest{GitOpsScope: scope, Name: "synthetic-source", Kind: sourceKind, URL: sourceURL}); err != nil {
		t.Fatal(err)
	}
	if scope.Controller == "argocd" {
		api.objects["argoproj.io/appprojects/argocd/synthetic-project"] = object(t, `{"apiVersion":"argoproj.io/v1alpha1","kind":"AppProject","metadata":{"name":"synthetic-project","namespace":"argocd","uid":"synthetic-project-uid","resourceVersion":"1"},"spec":{"sourceRepos":["`+sourceURL+`"],"destinations":[{"server":"https://kubernetes.default.svc","namespace":"default"}],"clusterResourceWhitelist":[]}}`)
	}
	return GitOpsRequest{GitOpsScope: scope, Action: "install", Name: "synthetic-app", Kind: kind, Source: "synthetic-source", Revision: revision, Path: "./deploy", Chart: "synthetic-chart", ServiceAccount: "synthetic-reconciler", Project: "synthetic-project", Renderer: "kustomize"}
}

func TestGitOpsInstallPreviewsAndCreatesSuspendedNativeApplications(t *testing.T) {
	for _, entry := range []struct{ controller, kind, source string }{
		{"flux", "Kustomization", "git"}, {"flux", "HelmRelease", "helm"},
		{"argocd", "Application", "git"}, {"argocd", "Application", "helm"},
	} {
		t.Run(entry.controller+"/"+entry.kind+"/"+entry.source, func(t *testing.T) {
			manager, api, ops, scope := gitOpsFixture(t, entry.controller)
			request := gitOpsInstallRequest(t, manager, api, scope, entry.kind, entry.source)
			preview, err := manager.PreviewGitOps(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Differences) == 0 || len(preview.Warnings) == 0 {
				t.Fatalf("missing meaningful native preview: %+v", preview)
			}
			if _, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, false, false); err == nil {
				t.Fatal("unconfirmed preview was accepted")
			}
			op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
				t.Fatalf("suspended install failed: %+v", result)
			}
			target := gitOpsTarget(scope, entry.kind, request.Name)
			api.mu.Lock()
			defer api.mu.Unlock()
			root := api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/"+target.Name]
			if root == nil {
				t.Fatal("native GitOps application was not created")
			}
			if entry.controller == "flux" {
				suspended, _, _ := unstructured.NestedBool(root.Object, "spec", "suspend")
				if !suspended {
					t.Fatal("Flux reconciled without explicit enablement")
				}
			} else if root.GetAnnotations()["argocd.argoproj.io/skip-reconcile"] != "true" {
				t.Fatal("Argo CD reconciled without explicit enablement")
			}
			for _, request := range api.requests {
				if request.Query.Get("force") == "true" {
					t.Fatal("GitOps used force ownership")
				}
			}
			data, _ := json.Marshal(root)
			if strings.Contains(string(data), "password") {
				t.Fatal("root application embeds credentials")
			}
		})
	}
}
