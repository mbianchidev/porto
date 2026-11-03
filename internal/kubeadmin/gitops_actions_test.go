package kubeadmin

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func installGitOpsFixture(t *testing.T, controller, kind, source string) (*Manager, *fakeAPI, GitOpsScope) {
	t.Helper()
	manager, api, ops, scope := gitOpsFixture(t, controller)
	request := gitOpsInstallRequest(t, manager, api, scope, kind, source)
	preview, err := manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatalf("fixture install: %+v", result)
	}
	return manager, api, scope
}

func simulateFluxReconciliation(api *fakeAPI, scope GitOpsScope, failure string) {
	api.afterApply = func(value *unstructured.Unstructured) {
		if value.GetKind() != "GitRepository" && value.GetKind() != "Kustomization" || gitOpsBool(value, "spec", "suspend") {
			return
		}
		value.SetGeneration(1)
		revision := gitOpsString(value, "spec", "ref", "commit")
		if value.GetKind() == "Kustomization" {
			source := api.objects["source.toolkit.fluxcd.io/gitrepositories/"+scope.Namespace+"/"+value.GetName()+"-source"]
			revision = gitOpsString(source, "spec", "ref", "commit")
		}
		reason, ready := "ReconciliationSucceeded", "True"
		if failure != "" && value.GetKind() == "Kustomization" {
			reason, ready = failure, "False"
		}
		value.Object["status"] = map[string]any{
			"observedGeneration": int64(1), "lastAppliedRevision": "main@sha1:" + revision,
			"lastHandledReconcileAt": value.GetAnnotations()["reconcile.fluxcd.io/requestedAt"],
			"artifact":               map[string]any{"revision": "main@sha1:" + revision},
			"conditions":             []any{map[string]any{"type": "Ready", "status": ready, "reason": reason, "observedGeneration": int64(1)}},
		}
	}
}

func TestGitOpsManualReconcileAndRecoveryRunOnceThenSuspend(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "flux", "Kustomization", "git")
	simulateFluxReconciliation(api, scope, "")
	for _, action := range []string{"reconcile", "recover"} {
		current, err := manager.GitOpsApplication(context.Background(), scope, "Kustomization", "synthetic-app")
		if err != nil {
			t.Fatal(err)
		}
		revision := ""
		if action == "recover" {
			revision = strings.Repeat("b", 40)
		}
		request := GitOpsRequest{GitOpsScope: scope, Kind: current.Kind, Name: current.Name, UID: current.Target.UID, ResourceVersion: current.Target.ResourceVersion, Action: action, Revision: revision}
		preview, err := manager.PreviewGitOps(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if result := waitAdmin(t, manager.operations, op.ID); result.Status != "succeeded" {
			t.Fatalf("one-shot %s failed: %+v", action, result)
		}
		current, err = manager.GitOpsApplication(context.Background(), scope, current.Kind, current.Name)
		if err != nil {
			t.Fatal(err)
		}
		if current.Phase != "suspended" || current.AppliedRevision != current.DesiredRevision || current.Health != "healthy" {
			t.Fatalf("one-shot did not converge and pause: %+v", current)
		}
		if action == "recover" && current.DesiredRevision != revision {
			t.Fatal("recovery did not pin the requested desired revision")
		}
	}
}

func TestGitOpsFailedReadinessIsPartialAndOneShotStillSuspends(t *testing.T) {
	manager, api, scope := installGitOpsFixture(t, "flux", "Kustomization", "git")
	simulateFluxReconciliation(api, scope, "HealthCheckFailed")
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
	result := waitAdmin(t, manager.operations, op.ID)
	if result.Status != "partial" || !strings.Contains(result.Error, "workload-health") {
		t.Fatalf("failed readiness looked successful: %+v", result)
	}
	current, err = manager.GitOpsApplication(context.Background(), scope, current.Kind, current.Name)
	if err != nil || current.Phase != "suspended" {
		t.Fatalf("failed one-shot was left running: %+v %v", current, err)
	}
}
