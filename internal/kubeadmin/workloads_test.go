package kubeadmin

import (
	"context"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"strings"
	"testing"
	"time"
)

func TestDeletionFinalizersAndCanceledRolloutStayExplicitlyPartial(t *testing.T) {
	manager, api, ops := adminFixture(t)
	api.objects["/configmaps/default/finalized"] = object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"finalized","namespace":"default","uid":"finalized-uid","resourceVersion":"1","finalizers":["fixtures.example.invalid/hold"]}}`)
	request := WorkloadRequest{Target: Target{Context: "porto-synthetic", Namespace: "default", Version: "v1", Resource: "configmaps", Name: "finalized", UID: "finalized-uid", ResourceVersion: "1"}, Action: "delete", Confirm: true}
	op, err := manager.Workload(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "partial" || !strings.Contains(result.Error, "finalizers") {
		t.Fatalf("finalizer deletion appeared complete: %+v", result)
	}
	api.mu.Lock()
	api.objects["apps/deployments/default/slow"] = object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"slow","namespace":"default","uid":"slow-uid","resourceVersion":"1","generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"readyReplicas":1,"updatedReplicas":1}}`)
	api.holdRollout = true
	api.mutated = make(chan struct{}, 1)
	mutated := api.mutated
	api.mu.Unlock()
	request = WorkloadRequest{Target: Target{Context: "porto-synthetic", Namespace: "default", Group: "apps", Version: "v1", Resource: "deployments", Name: "slow", UID: "slow-uid", ResourceVersion: "1"}, Action: "restart", Confirm: true}
	op, err = manager.Workload(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-mutated:
	case <-time.After(time.Second):
		t.Fatal("synthetic restart did not reach the API")
	}
	if err := ops.Cancel(op.ID); err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "partial" {
		t.Fatalf("canceled accepted rollout appeared completed: %+v", result)
	}
}

func TestControllerRollbackNeverUsesAnotherWorkloadUID(t *testing.T) {
	manager, api, ops := adminFixture(t)
	api.objects["apps/deployments/default/synthetic"] = object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"synthetic","namespace":"default","uid":"deployment-uid","resourceVersion":"1","generation":1},"spec":{"replicas":1,"template":{"metadata":{"labels":{"app":"synthetic"}},"spec":{"containers":[{"name":"app","image":"registry.invalid/synthetic:new"}]}}},"status":{"observedGeneration":1,"readyReplicas":1,"updatedReplicas":1}}`)
	api.objects["apps/replicasets/default/owned"] = object(t, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"owned","namespace":"default","uid":"owned-rs","resourceVersion":"1","annotations":{"deployment.kubernetes.io/revision":"1"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"synthetic","uid":"deployment-uid","controller":true}]},"spec":{"template":{"metadata":{"labels":{"app":"synthetic","pod-template-hash":"old-hash"}},"spec":{"containers":[{"name":"app","image":"registry.invalid/synthetic:old"}]}}}}`)
	api.objects["apps/replicasets/default/foreign"] = object(t, `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"foreign","namespace":"default","uid":"foreign-rs","resourceVersion":"1","annotations":{"deployment.kubernetes.io/revision":"2"},"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"synthetic","uid":"another-deployment","controller":true}]},"spec":{"template":{"spec":{"containers":[{"name":"app","image":"registry.invalid/foreign:wrong"}]}}}}`)
	target := Target{Context: "porto-synthetic", Namespace: "default", Group: "apps", Version: "v1", Resource: "deployments", Name: "synthetic", UID: "deployment-uid", ResourceVersion: "1"}
	_ = unstructured.SetNestedMap(api.objects["apps/deployments/default/synthetic"].Object, map[string]any{"new": "only"}, "spec", "template", "spec", "nodeSelector")
	history, err := manager.RolloutHistory(context.Background(), target)
	if err != nil || len(history) != 1 || history[0].Revision != 1 {
		t.Fatalf("foreign controller history was mixed: %+v %v", history, err)
	}
	op, err := manager.Workload(context.Background(), WorkloadRequest{Target: target, Action: "rollback", Revision: 1, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	api.mu.Lock()
	template, _, _ := unstructured.NestedMap(api.objects["apps/deployments/default/synthetic"].Object, "spec", "template")
	if _, found, _ := unstructured.NestedMap(template, "spec", "nodeSelector"); found {
		t.Fatal("rollback merged current-only pod fields instead of restoring the exact retained template")
	}
	if labels, found, _ := unstructured.NestedMap(template, "metadata", "labels"); found {
		if _, present := labels["pod-template-hash"]; present {
			t.Fatal("controller-generated hash label was restored")
		}
	}
	api.mu.Unlock()
}
