package kubeadmin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsStatusSeparatesSourceRenderReconcileAndHealthFailures(t *testing.T) {
	for _, entry := range []struct{ name, controller, reason, stage string }{
		{"source", "flux", "AuthenticationFailed", "source"},
		{"render", "flux", "BuildFailed", "render/admission"},
		{"apply", "flux", "ReconciliationFailed", "reconciliation"},
		{"workload", "flux", "HealthCheckFailed", "workload-health"},
		{"synced-unhealthy", "argocd", "", "workload-health"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			manager, api, _, scope := gitOpsFixture(t, entry.controller)
			kind := "Kustomization"
			if entry.controller == "argocd" {
				kind = "Application"
			}
			request := gitOpsInstallRequest(t, manager, api, scope, kind, "git")
			objects, _, _, err := manager.gitOpsApplicationObjects(context.Background(), mustGitOpsClient(t, manager, scope), request)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range objects {
				value.SetUID("synthetic-root")
				value.SetResourceVersion("1")
				value.SetGeneration(1)
				if value.GetKind() == "GitRepository" {
					value.Object["status"] = map[string]any{"observedGeneration": int64(1), "artifact": map[string]any{"revision": "main@sha1:" + request.Revision}, "conditions": []any{map[string]any{"type": "Ready", "status": "True", "observedGeneration": int64(1)}}}
					if entry.stage == "source" {
						value.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": entry.reason, "message": "synthetic-private-token"}}}
					}
				} else if entry.controller == "flux" {
					_ = unstructured.SetNestedField(value.Object, false, "spec", "suspend")
					value.Object["status"] = map[string]any{"observedGeneration": int64(1), "lastAppliedRevision": "main@sha1:" + request.Revision, "conditions": []any{map[string]any{"type": "Ready", "status": "False", "observedGeneration": int64(1), "reason": entry.reason, "message": "synthetic-private-token"}}}
				} else {
					value.SetAnnotations(map[string]string{})
					value.Object["status"] = map[string]any{"sync": map[string]any{"status": "Synced", "revision": request.Revision}, "health": map[string]any{"status": "Degraded", "message": "synthetic-private-token"}, "operationState": map[string]any{"phase": "Succeeded"}}
				}
				target, _, err := mustGitOpsClient(t, manager, scope).targetForObject(scope.Context, value.GetNamespace(), value)
				if err != nil {
					t.Fatal(err)
				}
				api.objects[target.Group+"/"+target.Resource+"/"+target.Namespace+"/"+target.Name] = value
			}
			state, err := manager.GitOpsApplication(context.Background(), scope, kind, request.Name)
			if err != nil {
				t.Fatal(err)
			}
			if state.FailureStage != entry.stage || state.Health == "healthy" {
				t.Fatalf("failure stage/health was conflated: %+v", state)
			}
			data, _ := json.Marshal(state)
			if strings.Contains(string(data), "synthetic-private-token") {
				t.Fatal("controller diagnostics exposed a credential")
			}
		})
	}
}

func mustGitOpsClient(t *testing.T, manager *Manager, scope GitOpsScope) *session {
	t.Helper()
	client, err := manager.client(scope.Context)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
