package kubeadmin

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsArgoOneShotResuspendsAfterNativeOperationClears(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "argocd", "Application", "git")
	var timer *time.Timer
	api.afterApply = func(value *unstructured.Unstructured) {
		if value.GetKind() != "Application" || value.GetAnnotations()["argocd.argoproj.io/skip-reconcile"] == "true" {
			return
		}
		operation, found, _ := unstructured.NestedMap(value.Object, "operation")
		if !found {
			return
		}
		timer = time.AfterFunc(50*time.Millisecond, func() {
			api.mu.Lock()
			defer api.mu.Unlock()
			root := api.objects["argoproj.io/applications/argocd/synthetic-app"]
			revision := gitOpsString(root, "spec", "source", "targetRevision")
			delete(root.Object, "operation")
			unstructured.RemoveNestedField(root.Object, "spec", "source", "kustomize")
			root.SetGeneration(root.GetGeneration() + 1)
			root.Object["status"] = map[string]any{"sync": map[string]any{"status": "Synced", "revision": revision, "comparedTo": map[string]any{"source": map[string]any{"targetRevision": revision}}}, "health": map[string]any{"status": "Healthy"}, "operationState": map[string]any{"phase": "Succeeded", "operation": operation, "syncResult": map[string]any{"revision": revision}}}
		})
	}
	t.Cleanup(func() {
		api.mu.Lock()
		defer api.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
	})
	current, err := manager.GitOpsApplication(context.Background(), scope, "Application", "synthetic-app")
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.PreviewGitOps(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "reconcile", Kind: current.Kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion})
	if err != nil {
		t.Fatal(err)
	}
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result := waitAdmin(t, manager.operations, op.ID)
	if result.Status != "succeeded" {
		t.Fatalf("native operation cleanup prevented safe suspension: %+v", result)
	}
	current, err = manager.GitOpsApplication(context.Background(), scope, current.Kind, current.Name)
	if err != nil || current.Phase != "suspended" {
		t.Fatalf("one-shot application remained active: %+v %v", current, err)
	}
}

func TestGitOpsArgoSpecHashOnlyNormalizesEmptyRendererOptions(t *testing.T) {
	first := object(t, `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"name":"synthetic"},"spec":{"source":{"repoURL":"https://git.example.invalid/synthetic.git","path":"deploy","kustomize":{}}}}`)
	normalized := first.DeepCopy()
	unstructured.RemoveNestedField(normalized.Object, "spec", "source", "kustomize")
	if gitOpsSpecHash(first) != gitOpsSpecHash(normalized) {
		t.Fatal("Argo CD's empty renderer normalization changed semantic identity")
	}
	changed := normalized.DeepCopy()
	if err := unstructured.SetNestedMap(changed.Object, map[string]any{"namePrefix": "different-"}, "spec", "source", "kustomize"); err != nil {
		t.Fatal(err)
	}
	if gitOpsSpecHash(first) == gitOpsSpecHash(changed) {
		t.Fatal("a meaningful renderer change was hidden")
	}
}
