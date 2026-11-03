package kubeadmin

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsOwnershipGuardBindsOnlyTheSelectedNamespaceAndFailsClosed(t *testing.T) {
	scope := GitOpsScope{Context: "porto-synthetic", Namespace: "synthetic-apps", Controller: "flux", ControllerNamespace: "flux-system"}
	objects := gitOpsOwnershipObjects(scope)
	if len(objects) != 2 {
		t.Fatal("ownership protection needs both policy and binding")
	}
	policy, binding := objects[0], objects[1]
	if gitOpsString(policy, "spec", "failurePolicy") != "Fail" {
		t.Fatal("ownership admission does not fail closed")
	}
	if gitOpsString(binding, "spec", "matchResources", "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name") != scope.Namespace {
		t.Fatal("ownership protection escaped selected namespace")
	}
	actions, _, _ := unstructured.NestedStringSlice(binding.Object, "spec", "validationActions")
	if len(actions) != 1 || actions[0] != "Deny" {
		t.Fatal("ownership binding does not enforce denial")
	}
}

func TestGitOpsActivationRechecksNamespaceOwnershipProtection(t *testing.T) {
	manager, api, ops, scope := gitOpsFixture(t, "flux")
	request := gitOpsInstallRequest(t, manager, api, scope, "Kustomization", "git")
	preview, err := manager.PreviewGitOps(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	binding := gitOpsOwnershipObjects(scope)[1]
	delete(api.objects, "admissionregistration.k8s.io/validatingadmissionpolicybindings//"+binding.GetName())
	op, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true)
	if err != nil {
		t.Fatal(err)
	}
	result := waitAdmin(t, ops, op.ID)
	if result.Status != "failed" || api.applyCount != 0 {
		t.Fatalf("missing ownership binding did not stop activation: %+v", result)
	}
}
