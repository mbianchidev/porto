package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsBootstrapPinsBundlesAndSeparatesInstallationAuthority(t *testing.T) {
	document := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: flux-system\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: source-controller\n  namespace: flux-system\nspec:\n  replicas: 1\n  template:\n    spec:\n      containers:\n      - name: manager\n        image: ghcr.io/fluxcd/source-controller:v1.10.1\n")
	sum := sha256.Sum256(document)
	if err := verifyGitOpsBundle(document, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if err := verifyGitOpsBundle(append(document, []byte("# changed")...), hex.EncodeToString(sum[:])); err == nil {
		t.Fatal("modified bootstrap bundle accepted")
	}
	manager, api, _, scope := gitOpsFixture(t, "flux")
	objects, err := gitOpsBootstrapObjects(scope, document)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("bootstrap changed selected component inventory: %d", len(objects))
	}
	for _, object := range objects {
		if object.GetKind() != "Namespace" && object.GetNamespace() != scope.ControllerNamespace {
			t.Fatal("bootstrap escaped selected controller namespace")
		}
		if object.GetAnnotations()[gitOpsInstallation] == "" {
			t.Fatal("bootstrap omitted installation provenance")
		}
	}
	api.mu.Lock()
	for key := range api.objects {
		if strings.HasPrefix(key, "apps/") {
			delete(api.objects, key)
		}
	}
	api.denied["create/namespaces"] = true
	api.mu.Unlock()
	_, err = manager.previewGitOpsControllerObjects(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap", Kind: "Controller", Name: "flux"}, objects)
	if err == nil {
		t.Fatal("read-only bootstrap identity was accepted")
	}
	api.mu.Lock()
	delete(api.denied, "create/namespaces")
	api.mu.Unlock()
	preview, err := manager.previewGitOpsControllerObjects(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap", Kind: "Controller", Name: "flux"}, objects)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Consequences) == 0 {
		t.Fatal("controller installation did not require RBAC/CRD consequence acknowledgement")
	}
	if _, err := manager.ApplyGitOps(context.Background(), scope, preview.Token, true, true); err == nil {
		t.Fatal("application authority consumed a controller bootstrap token")
	}
}

func TestGitOpsBootstrapPreservesExistingNamespaceButRefusesForeignResources(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	api.objects["/namespaces//flux-system"] = object(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"flux-system","uid":"namespace-uid","resourceVersion":"1"}}`)
	objects := []*unstructured.Unstructured{
		object(t, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"flux-system","labels":{"porto.dev/gitops-owner":"synthetic-installation"}}}`),
		object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"source-controller","namespace":"flux-system","labels":{"porto.dev/gitops-owner":"synthetic-installation"}}}`),
	}
	if _, err := manager.previewGitOpsControllerObjects(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap", Kind: "Controller", Name: "flux"}, objects); err == nil {
		t.Fatal("a foreign controller installation was overwritten")
	}
}

func TestGitOpsControllerUpdatesDoNotClaimGeneratedSecretFields(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "argocd")
	owner := gitOpsInstallationOwner(scope)
	api.objects["/secrets/argocd/argocd-secret"] = object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"argocd-secret","namespace":"argocd","uid":"generated-secret","resourceVersion":"1","labels":{"porto.dev/gitops-owner":"`+owner+`"}},"data":{"tls.key":"c3ludGhldGljLWdlbmVyYXRlZC1rZXk="}}`)
	desired := object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"argocd-secret","namespace":"argocd","labels":{"porto.dev/gitops-owner":"`+owner+`"}}}`)
	if _, err := manager.previewGitOpsControllerObjects(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap", Kind: "Controller", Name: "argocd"}, []*unstructured.Unstructured{desired}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, request := range api.requests {
		if request.Path == "/api/v1/namespaces/argocd/secrets/argocd-secret" && request.Method != "GET" {
			found = true
			if request.Method != "PATCH" || request.Query.Get("force") == "true" || strings.Contains(string(request.Body), "tls.key") {
				t.Fatalf("controller update claimed generated Secret values: %s %s", request.Method, request.Body)
			}
		}
	}
	if !found {
		t.Fatal("missing server-side dry run for controller-owned Secret")
	}
}

func TestGitOpsBootstrapDefersNamespacedAdmissionUntilReviewedNamespaceCreation(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	api.strictNamespaces = true
	for key := range api.objects {
		if strings.HasPrefix(key, "apps/") {
			delete(api.objects, key)
		}
	}
	document := []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: flux-system\n---\napiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: source-controller\n  namespace: flux-system\nspec:\n  template:\n    spec:\n      containers:\n      - name: manager\n        image: ghcr.io/fluxcd/source-controller:v1.10.1\n")
	objects, err := gitOpsBootstrapObjects(scope, document)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.previewGitOpsControllerObjects(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "bootstrap", Kind: "Controller", Name: "flux"}, objects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(preview.Warnings, " "), "admission is deferred") {
		t.Fatal("unavailable namespaced admission was presented as fully validated")
	}
	if api.applyCount != 0 || api.objects["/namespaces//flux-system"] != nil {
		t.Fatal("bootstrap preview created a namespace")
	}
}
