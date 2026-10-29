package kubeadmin

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestApplyToAbsentObjectCannotRetargetAConcurrentCreator(t *testing.T) {
	manager, api, ops := adminFixture(t)
	preview, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: synthetic-race\ndata:\n  value: planned\n", Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.concurrentCreate = true
	api.mu.Unlock()
	op, err := manager.ApplyManifest(context.Background(), "porto-synthetic", "default", preview.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "failed" {
		t.Fatalf("concurrently created object was retargeted: %+v", result)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	current := api.objects["/configmaps/default/synthetic-race"]
	value, _, _ := unstructured.NestedString(current.Object, "data", "value")
	if string(current.GetUID()) != "concurrent-uid" || value != "concurrent-owner" {
		t.Fatal("the concurrent creator's data was overwritten")
	}
}
