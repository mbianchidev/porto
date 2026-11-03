package kubeadmin

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsAdoptionStagesDirectWriterExclusionAndVerifiesNativeOwnership(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
	target := Target{Context: scope.Context, Namespace: scope.Namespace, Version: "v1", Resource: "configmaps", Name: "synthetic-existing", UID: "existing-uid", ResourceVersion: "1"}
	api.objects["/configmaps/default/synthetic-existing"] = object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic-existing","namespace":"default","uid":"existing-uid","resourceVersion":"1"},"data":{"setting":"synthetic"}}`)
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
		t.Fatalf("staging adoption: %+v", result)
	}
	if _, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: scope.Context, Namespace: scope.Namespace, Manifest: `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic-existing"},"data":{"setting":"changed"}}`}); err == nil {
		t.Fatal("staged resource still admits direct writers")
	}
	current, err := manager.GitOpsApplication(context.Background(), scope, request.Kind, request.Name)
	if err != nil {
		t.Fatal(err)
	}
	verify := GitOpsRequest{GitOpsScope: scope, Kind: current.Kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion, Action: "verify-adoption"}
	if _, err := manager.PreviewGitOps(context.Background(), verify); err == nil {
		t.Fatal("unobserved native adoption was marked verified")
	}
	api.mu.Lock()
	root := api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"]
	root.Object["status"] = map[string]any{"observedGeneration": root.GetGeneration(), "lastAppliedRevision": "main@sha1:" + request.Revision, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": root.GetGeneration()}}, "inventory": map[string]any{"entries": []any{map[string]any{"id": "default_synthetic-existing__ConfigMap", "v": "v1"}}}}
	source := api.objects["source.toolkit.fluxcd.io/gitrepositories/default/synthetic-app-source"]
	source.Object["status"] = map[string]any{"observedGeneration": source.GetGeneration(), "artifact": map[string]any{"revision": "main@sha1:" + request.Revision}, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": source.GetGeneration()}}}
	resource := api.objects["/configmaps/default/synthetic-existing"]
	resource.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": request.Name, "kustomize.toolkit.fluxcd.io/namespace": scope.Namespace})
	api.mu.Unlock()
	preview, err = manager.PreviewGitOps(context.Background(), verify)
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatalf("verifying adoption: %+v", result)
	}
	current, err = manager.GitOpsApplication(context.Background(), scope, request.Kind, request.Name)
	if err != nil || current.Adoption != "verified" {
		t.Fatalf("adoption not verified: %+v %v", current, err)
	}
}

func TestGitOpsOrphanDeletionPreservesWorkloadsAndUnrelatedApplications(t *testing.T) {
	for _, controller := range []string{"flux", "argocd"} {
		t.Run(controller, func(t *testing.T) {
			kind := "Kustomization"
			if controller == "argocd" {
				kind = "Application"
			}
			manager, api, scope := installGitOpsFixture(t, controller, kind, "git")
			api.objects["/configmaps/default/synthetic-data"] = object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic-data","namespace":"default","uid":"data-uid","resourceVersion":"1"}}`)
			target := gitOpsTarget(scope, kind, "synthetic-app")
			api.mu.Lock()
			other := api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/"+target.Name].DeepCopy()
			other.SetName("synthetic-unrelated")
			other.SetUID("unrelated-uid")
			if controller == "flux" {
				_ = unstructured.SetNestedField(other.Object, "synthetic-unrelated-source", "spec", "sourceRef", "name")
			}
			api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/synthetic-unrelated"] = other
			api.mu.Unlock()
			current, err := manager.GitOpsApplication(context.Background(), scope, kind, "synthetic-app")
			if err != nil {
				t.Fatal(err)
			}
			preview, err := manager.PreviewGitOps(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "delete", DeletePolicy: "orphan", Kind: kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion})
			if err != nil {
				t.Fatal(err)
			}
			op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if result := waitAdmin(t, manager.operations, op.ID); result.Status != "succeeded" {
				t.Fatalf("orphan delete: %+v", result)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.objects["/configmaps/default/synthetic-data"] == nil || api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/synthetic-unrelated"] == nil {
				t.Fatal("orphan deletion touched unrelated resources")
			}
			if api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/synthetic-app"] != nil {
				t.Fatal("application was not deleted")
			}
		})
	}
}

func TestGitOpsPruningRequiresAdditionalDataAcknowledgement(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "flux", "Kustomization", "git")
	api.mu.Lock()
	root := api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"]
	root.Object["status"] = map[string]any{"inventory": map[string]any{"entries": []any{map[string]any{"id": "default_synthetic-volume__PersistentVolumeClaim", "v": "v1"}}}}
	api.objects["/persistentvolumeclaims/default/synthetic-volume"] = object(t, `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"synthetic-volume","namespace":"default","uid":"synthetic-volume-uid","resourceVersion":"1","labels":{"kustomize.toolkit.fluxcd.io/name":"synthetic-app","kustomize.toolkit.fluxcd.io/namespace":"default"}}}`)
	api.mu.Unlock()
	current, err := manager.GitOpsApplication(context.Background(), scope, "Kustomization", "synthetic-app")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.PreviewGitOps(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "delete", DeletePolicy: "prune", Kind: current.Kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Consequences) == 0 || !strings.Contains(strings.Join(preview.Consequences, " "), "PersistentVolumeClaim") {
		t.Fatalf("storage consequence missing: %+v", preview)
	}
	if _, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, false); err == nil {
		t.Fatal("unacknowledged storage pruning was accepted")
	}
}
