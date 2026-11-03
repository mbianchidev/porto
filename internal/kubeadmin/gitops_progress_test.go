package kubeadmin

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsReconciliationWaitsForInitialSourceArtifactInsteadOfSuspendingEarly(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "flux", "Kustomization", "git")
	var timer *time.Timer
	api.afterApply = func(value *unstructured.Unstructured) {
		if value.GetKind() != "Kustomization" || gitOpsBool(value, "spec", "suspend") {
			return
		}
		nonce := value.GetAnnotations()["reconcile.fluxcd.io/requestedAt"]
		value.SetGeneration(1)
		value.Object["status"] = map[string]any{"observedGeneration": int64(1), "lastHandledReconcileAt": nonce, "conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "ArtifactFailed", "observedGeneration": int64(1)}}}
		timer = time.AfterFunc(50*time.Millisecond, func() {
			api.mu.Lock()
			defer api.mu.Unlock()
			source := api.objects["source.toolkit.fluxcd.io/gitrepositories/default/synthetic-app-source"]
			source.Object["status"] = map[string]any{"observedGeneration": source.GetGeneration(), "artifact": map[string]any{"revision": "main@sha1:" + strings.Repeat("a", 40)}, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": source.GetGeneration()}}}
			root := api.objects["kustomize.toolkit.fluxcd.io/kustomizations/default/synthetic-app"]
			root.Object["status"] = map[string]any{"observedGeneration": int64(1), "lastAppliedRevision": "main@sha1:" + strings.Repeat("a", 40), "lastHandledReconcileAt": nonce, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}}
		})
	}
	t.Cleanup(func() {
		api.mu.Lock()
		defer api.mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
	})
	current, err := manager.GitOpsApplication(context.Background(), scope, "Kustomization", "synthetic-app")
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
	if result := waitAdmin(t, manager.operations, op.ID); result.Status != "succeeded" {
		t.Fatalf("transient source acquisition ended the attempt early: %+v", result)
	}
}
