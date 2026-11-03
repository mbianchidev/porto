package kubeadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/operations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func gitOpsFixture(t *testing.T, controller string) (*Manager, *fakeAPI, *operations.Manager, GitOpsScope) {
	t.Helper()
	manager, api, ops := adminFixture(t)
	scope := GitOpsScope{Context: "porto-synthetic", Namespace: "default", Controller: controller, ControllerNamespace: "flux-system"}
	if controller == "argocd" {
		scope.ControllerNamespace = "argocd"
	}
	definitions := []struct {
		group, version, resource, kind string
		fields                         []string
	}{
		{"source.toolkit.fluxcd.io", "v1", "gitrepositories", "GitRepository", []string{"spec.url", "spec.ref.commit", "spec.secretRef.name", "spec.suspend"}},
		{"source.toolkit.fluxcd.io", "v1", "helmrepositories", "HelmRepository", []string{"spec.url", "spec.secretRef.name", "spec.suspend"}},
		{"kustomize.toolkit.fluxcd.io", "v1", "kustomizations", "Kustomization", []string{"spec.sourceRef.name", "spec.path", "spec.suspend", "spec.serviceAccountName", "spec.prune", "spec.deletionPolicy"}},
		{"helm.toolkit.fluxcd.io", "v2", "helmreleases", "HelmRelease", []string{"spec.chart.spec.chart", "spec.chart.spec.sourceRef.name", "spec.suspend", "spec.serviceAccountName", "spec.driftDetection.mode"}},
		{"argoproj.io", "v1alpha1", "applications", "Application", []string{"spec.source.repoURL", "spec.source.targetRevision", "spec.project", "spec.destination.namespace", "spec.syncPolicy.automated.enabled", "operation.sync.revision"}},
		{"argoproj.io", "v1alpha1", "appprojects", "AppProject", []string{"spec.sourceRepos", "spec.destinations"}},
	}
	resourceLists := map[string][]metav1.APIResource{}
	for _, definition := range definitions {
		groupVersion := definition.group + "/" + definition.version
		resourceLists[groupVersion] = append(resourceLists[groupVersion], metav1.APIResource{Name: definition.resource, Kind: definition.kind, Namespaced: true, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}})
		root := map[string]any{"type": "object", "properties": map[string]any{}}
		for _, field := range definition.fields {
			node := root
			for _, segment := range strings.Split(field, ".") {
				properties := node["properties"].(map[string]any)
				child, ok := properties[segment].(map[string]any)
				if !ok {
					child = map[string]any{"type": "object", "properties": map[string]any{}}
					properties[segment] = child
				}
				node = child
			}
		}
		crd := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": definition.resource + "." + definition.group, "uid": definition.resource + "-crd", "resourceVersion": "1"},
			"spec":     map[string]any{"group": definition.group, "scope": "Namespaced", "names": map[string]any{"plural": definition.resource, "kind": definition.kind}, "versions": []any{map[string]any{"name": definition.version, "served": true, "storage": true, "schema": map[string]any{"openAPIV3Schema": root}}}},
			"status":   map[string]any{"conditions": []any{map[string]any{"type": "Established", "status": "True"}}},
		}}
		api.objects["apiextensions.k8s.io/customresourcedefinitions//"+crd.GetName()] = crd
	}
	resourceLists["apiextensions.k8s.io/v1"] = []metav1.APIResource{{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition", Verbs: []string{"get", "list", "create", "patch", "update", "delete"}}}
	resourceLists["admissionregistration.k8s.io/v1"] = []metav1.APIResource{
		{Name: "validatingadmissionpolicies", Kind: "ValidatingAdmissionPolicy", Verbs: []string{"get", "list", "create", "patch", "update", "delete"}},
		{Name: "validatingadmissionpolicybindings", Kind: "ValidatingAdmissionPolicyBinding", Verbs: []string{"get", "list", "create", "patch", "update", "delete"}},
	}
	for _, policy := range gitOpsOwnershipObjects(scope) {
		policy.SetUID(types.UID("synthetic-policy-" + policy.GetName()))
		policy.SetResourceVersion("1")
		policy.SetGeneration(1)
		policy.Object["status"] = map[string]any{"observedGeneration": int64(1), "typeChecking": map[string]any{"expressionWarnings": []any{}}}
		target := gitOpsPolicyTarget(scope, policy)
		api.objects[target.Group+"/"+target.Resource+"//"+target.Name] = policy
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if resources, ok := resourceLists[strings.TrimPrefix(r.URL.Path, "/apis/")]; ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: strings.TrimPrefix(r.URL.Path, "/apis/"), APIResources: resources})
			return
		}
		api.serve(w, r)
	}))
	t.Cleanup(server.Close)
	manager.configs = fakeConfigs{map[string]string{scope.Context: server.URL}}
	components := map[string]string{
		"source-controller":    "ghcr.io/fluxcd/source-controller:v1.10.1",
		"kustomize-controller": "ghcr.io/fluxcd/kustomize-controller:v1.10.1",
		"helm-controller":      "ghcr.io/fluxcd/helm-controller:v1.7.1",
	}
	if controller == "argocd" {
		components = map[string]string{"argocd-application-controller": "quay.io/argoproj/argocd:v3.5.7", "argocd-repo-server": "quay.io/argoproj/argocd:v3.5.7", "argocd-redis": "public.ecr.aws/docker/library/redis:8.2.3-alpine"}
	}
	for name, image := range components {
		kind, resource := "Deployment", "deployments"
		if name == "argocd-application-controller" {
			kind, resource = "StatefulSet", "statefulsets"
		}
		api.objects["apps/"+resource+"/"+scope.ControllerNamespace+"/"+name] = object(t, `{"apiVersion":"apps/v1","kind":"`+kind+`","metadata":{"name":"`+name+`","namespace":"`+scope.ControllerNamespace+`","uid":"`+name+`-uid","resourceVersion":"1","generation":1},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"manager","image":"`+image+`"}]}}},"status":{"observedGeneration":1,"readyReplicas":1,"availableReplicas":1,"updatedReplicas":1,"replicas":1,"currentRevision":"synthetic-revision","updateRevision":"synthetic-revision"}}`)
	}
	api.objects["/serviceaccounts/default/synthetic-reconciler"] = object(t, `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"synthetic-reconciler","namespace":"default","uid":"synthetic-sa","resourceVersion":"1"}}`)
	return manager, api, ops, scope
}

func TestGitOpsDiscoversControllersAndServedSchemasWithoutInstalling(t *testing.T) {
	for _, controller := range []string{"flux", "argocd"} {
		t.Run(controller, func(t *testing.T) {
			manager, api, _, scope := gitOpsFixture(t, controller)
			state, err := manager.GitOpsDiscover(context.Background(), scope)
			if err != nil {
				t.Fatal(err)
			}

			if !state.Installed || !state.Supported || !state.Ready || len(state.Capabilities) == 0 {
				t.Fatalf("installed controller/schema was not discovered: %+v", state)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, request := range api.requests {
				if request.Method != "GET" && !strings.Contains(request.Path, "selfsubjectaccessreviews") {
					t.Fatalf("discovery mutated cluster: %s %s", request.Method, request.Path)
				}
			}
		})
	}
}

func TestGitOpsSourcesKeepCredentialsScopedAndDeletionLocal(t *testing.T) {
	manager, api, _, scope := gitOpsFixture(t, "flux")
	password := "synthetic-gitops-private-token"
	request := GitOpsSourceRequest{GitOpsScope: scope, Name: "synthetic-source", Kind: "git", URL: "https://git.example.invalid/synthetic.git", Username: "synthetic-user", Password: &password}
	source, err := manager.SaveGitOpsSource(request)
	if err != nil {
		t.Fatal(err)
	}
	if !source.CredentialStored {
		t.Fatal("private source was not credential-store backed")
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(manager.root, "gitops-sources.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded)+string(metadata), password) {
		t.Fatal("source credential escaped encrypted storage")
	}
	other := scope
	other.Namespace = "other"
	sources, err := manager.GitOpsSources(other)
	if err != nil || len(sources) != 0 {
		t.Fatalf("source leaked into another namespace: %v %+v", err, sources)
	}
	request.URL, request.Password = "https://other.example.invalid/synthetic.git", nil
	if _, err := manager.SaveGitOpsSource(request); err == nil {
		t.Fatal("credentials were silently reused for a different URL")
	}
	if err := manager.DeleteGitOpsSource(scope, source.Name); err != nil {
		t.Fatal(err)
	}
	for _, request := range api.requests {
		if request.Method != "GET" && !strings.Contains(request.Path, "selfsubjectaccessreviews") {
			t.Fatalf("local registration removal mutated the cluster: %+v", request)
		}
	}
}
