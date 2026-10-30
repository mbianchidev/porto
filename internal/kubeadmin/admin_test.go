package kubeadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	"github.com/zalando/go-keyring"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type fakeVault struct {
	mu     sync.Mutex
	values map[string]string
}

func (v *fakeVault) Set(service, account, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[service+"/"+account] = secret
	return nil
}
func (v *fakeVault) Get(service, account string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (v *fakeVault) Delete(service, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, service+"/"+account)
	return nil
}

type fakeConfigs struct{ urls map[string]string }

func (c fakeConfigs) AdminConfig(contextName string) (*rest.Config, clientcmdapi.Config, error) {
	server := c.urls[contextName]
	if server == "" {
		return nil, clientcmdapi.Config{}, errors.New("explicit synthetic context required")
	}
	raw := clientcmdapi.Config{CurrentContext: contextName, Clusters: map[string]*clientcmdapi.Cluster{contextName: {Server: server}}, Contexts: map[string]*clientcmdapi.Context{contextName: {Cluster: contextName, AuthInfo: contextName}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{contextName: {Token: "synthetic-identity-token"}}}
	return &rest.Config{Host: server, BearerToken: "synthetic-identity-token", Timeout: time.Second, QPS: 1000, Burst: 1000, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}, raw, nil
}

type apiRequest struct {
	Method, Path string
	Query        url.Values
	Body         []byte
	Accept       string
}
type fakeAPI struct {
	mu               sync.Mutex
	objects          map[string]*unstructured.Unstructured
	requests         []apiRequest
	denied           map[string]bool
	failSecondApply  bool
	applyCount       int
	served           bool
	schemaHash       string
	holdRollout      bool
	mutated          chan struct{}
	concurrentCreate bool
	listPageSize     int
}

func object(t *testing.T, document string) *unstructured.Unstructured {
	t.Helper()
	var value unstructured.Unstructured
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		t.Fatal(err)
	}
	return &value
}

func newAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	api := &fakeAPI{objects: map[string]*unstructured.Unstructured{}, denied: map[string]bool{}, served: true, schemaHash: "one"}
	server := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(server.Close)
	return api, server
}

func status(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": reason, "code": code, "message": "synthetic-private-value must never escape an API error"})
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, apiRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body, Accept: r.Header.Get("Accept")})
	w.Header().Set("Content-Type", "application/json")
	respond := func(value any) { _ = json.NewEncoder(w).Encode(value) }
	if r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews" {
		var review struct {
			Spec struct {
				ResourceAttributes struct{ Verb, Resource, Subresource, Namespace string }
			}
		}
		if json.Unmarshal(body, &review) != nil {
			status(w, 400, "BadRequest")
			return
		}
		attributes := review.Spec.ResourceAttributes
		key := attributes.Verb + "/" + attributes.Resource
		if attributes.Subresource != "" {
			key += "/" + attributes.Subresource
		}
		respond(map[string]any{"apiVersion": "authorization.k8s.io/v1", "kind": "SelfSubjectAccessReview", "status": map[string]any{"allowed": !f.denied[key]}})
		return
	}
	switch r.URL.Path {
	case "/api":
		respond(map[string]any{"apiVersion": "v1", "kind": "APIVersions", "versions": []string{"v1"}})
		return
	case "/apis":
		respond(map[string]any{"apiVersion": "v1", "kind": "APIGroupList", "groups": []any{
			map[string]any{"name": "apps", "versions": []any{map[string]any{"groupVersion": "apps/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "apps/v1", "version": "v1"}},
			map[string]any{"name": "fixtures.example.invalid", "versions": []any{map[string]any{"groupVersion": "fixtures.example.invalid/v1", "version": "v1"}}, "preferredVersion": map[string]any{"groupVersion": "fixtures.example.invalid/v1", "version": "v1"}},
		}})
		return
	case "/api/v1":
		respond(metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: []string{"get", "list", "create", "patch", "delete"}},
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: []string{"get", "list", "create", "patch", "delete"}},
			{Name: "namespaces", Kind: "Namespace", Namespaced: false, Verbs: []string{"get", "list", "create", "patch", "delete"}},
		}})
		return
	case "/apis/apps/v1":
		respond(metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: []string{"get", "list", "create", "patch", "delete"}},
			{Name: "deployments/scale", Kind: "Scale", Namespaced: true, Verbs: []string{"get", "update"}},
			{Name: "replicasets", Kind: "ReplicaSet", Namespaced: true, Verbs: []string{"get", "list"}},
		}})
		return
	case "/apis/fixtures.example.invalid/v1":
		if !f.served {
			status(w, 404, "NotFound")
			return
		}
		respond(metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: "fixtures.example.invalid/v1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get", "list", "create", "patch", "delete"}}}})
		return
	case "/openapi/v3":
		respond(map[string]any{"paths": map[string]any{"apis/fixtures.example.invalid/v1": map[string]any{"serverRelativeURL": "/openapi/v3/apis/fixtures.example.invalid/v1?hash=" + f.schemaHash}}})
		return
	case "/openapi/v3/apis/fixtures.example.invalid/v1":
		respond(map[string]any{"openapi": "3.0.0", "components": map[string]any{"schemas": map[string]any{"FixtureWidget": map[string]any{"type": "object", "x-kubernetes-group-version-kind": []any{map[string]any{"group": "fixtures.example.invalid", "version": "v1", "kind": "Widget"}}, "properties": map[string]any{"spec": map[string]any{"type": "object", "properties": map[string]any{"count": map[string]any{"type": "integer"}}}}}}}})
		return
	}
	group, version, remainder := "", "v1", ""
	if strings.HasPrefix(r.URL.Path, "/api/v1/") {
		remainder = strings.TrimPrefix(r.URL.Path, "/api/v1/")
	} else {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/apis/"), "/")
		if len(parts) < 3 {
			status(w, 404, "NotFound")
			return
		}
		group, version, remainder = parts[0], parts[1], strings.Join(parts[2:], "/")
	}
	parts := strings.Split(remainder, "/")
	namespace, resource, name, subresource := "", parts[0], "", ""
	if parts[0] == "namespaces" && len(parts) >= 3 {
		namespace, resource = parts[1], parts[2]
		parts = parts[2:]
	}
	if len(parts) > 1 {
		name = parts[1]
	}
	if len(parts) > 2 {
		subresource = parts[2]
	}
	key := group + "/" + resource + "/" + namespace + "/" + name
	current := f.objects[key]
	if r.Method == "GET" && name == "" {
		items := []any{}
		keys := []string{}
		for objectKey := range f.objects {
			if strings.HasPrefix(objectKey, group+"/"+resource+"/"+namespace+"/") {
				keys = append(keys, objectKey)
			}
		}
		sort.Strings(keys)
		start := 0
		if token := r.URL.Query().Get("continue"); token != "" {
			parsed, err := strconv.Atoi(token)
			if err != nil || parsed < 0 || parsed > len(keys) {
				status(w, 400, "BadRequest")
				return
			}
			start = parsed
		}
		end, continuation := len(keys), ""
		if f.listPageSize > 0 && end-start > f.listPageSize {
			end = start + f.listPageSize
			continuation = strconv.Itoa(end)
		}
		for _, objectKey := range keys[start:end] {
			value := f.objects[objectKey]
			if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadata") {
				items = append(items, map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata", "metadata": value.Object["metadata"]})
			} else {
				items = append(items, value.Object)
			}
		}
		kind, apiVersion := "List", version
		if group != "" {
			apiVersion = group + "/" + version
		}
		if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadata") {
			kind, apiVersion = "PartialObjectMetadataList", "meta.k8s.io/v1"
		}
		respond(map[string]any{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"resourceVersion": "1", "continue": continuation}, "items": items})
		return
	}
	if r.Method == "GET" {
		if current == nil {
			status(w, 404, "NotFound")
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "PartialObjectMetadata") {
			respond(map[string]any{"apiVersion": "meta.k8s.io/v1", "kind": "PartialObjectMetadata", "metadata": current.Object["metadata"]})
			return
		}
		if subresource == "scale" {
			replicas, _, _ := unstructured.NestedInt64(current.Object, "spec", "replicas")
			respond(map[string]any{"apiVersion": "autoscaling/v1", "kind": "Scale", "metadata": current.Object["metadata"], "spec": map[string]any{"replicas": replicas}})
			return
		}
		respond(current.Object)
		return
	}
	jsonPatch := r.Method == "PATCH" && strings.Contains(r.Header.Get("Content-Type"), "application/json-patch+json")
	if jsonPatch && current != nil {
		var changes []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if json.Unmarshal(body, &changes) != nil {
			status(w, 400, "BadRequest")
			return
		}
		var template map[string]any
		for _, change := range changes {
			if change.Op == "test" {
				var value string
				_ = json.Unmarshal(change.Value, &value)
				if change.Path == "/metadata/uid" && value != string(current.GetUID()) || change.Path == "/metadata/resourceVersion" && value != current.GetResourceVersion() {
					status(w, 409, "Conflict")
					return
				}
			}
			if change.Op == "replace" && change.Path == "/spec/template" {
				_ = json.Unmarshal(change.Value, &template)
			}
		}
		body, _ = json.Marshal(map[string]any{"apiVersion": current.GetAPIVersion(), "kind": current.GetKind(), "metadata": current.Object["metadata"], "spec": map[string]any{"template": template}})
	}
	if r.Method == "PATCH" && current != nil {
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil {
			fields["apiVersion"], _ = json.Marshal(current.GetAPIVersion())
			fields["kind"], _ = json.Marshal(current.GetKind())
			body, _ = json.Marshal(fields)
		}
	}
	var desired unstructured.Unstructured
	if json.Unmarshal(body, &desired) != nil {
		status(w, 400, "BadRequest")
		return
	}
	if name == "" {
		name = desired.GetName()
		key = group + "/" + resource + "/" + namespace + "/" + name
		current = f.objects[key]
	}
	if r.Method == "DELETE" {
		if current == nil {
			status(w, 404, "NotFound")
			return
		}
		if len(current.GetFinalizers()) > 0 {
			now := metav1.Now()
			current.SetDeletionTimestamp(&now)
		} else {
			delete(f.objects, key)
		}
		respond(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
		return
	}
	if f.concurrentCreate && r.URL.Query().Get("dryRun") != "All" && current == nil && resource == "configmaps" {
		current = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": name, "namespace": namespace, "uid": "concurrent-uid", "resourceVersion": "30"}, "data": map[string]any{"value": "concurrent-owner"}}}
		f.objects[key] = current
		f.concurrentCreate = false
	}
	if r.Method == "POST" && current != nil {
		status(w, 409, "AlreadyExists")
		return
	}
	if expected := desired.GetResourceVersion(); expected != "" && current != nil && expected != current.GetResourceVersion() {
		status(w, 409, "Conflict")
		return
	}
	if resource == "widgets" {
		if value, found, _ := unstructured.NestedFieldNoCopy(desired.Object, "spec", "count"); found {
			if _, ok := value.(int64); !ok {
				status(w, 422, "Invalid")
				return
			}
		}
	}
	next := desired.DeepCopy()
	if current != nil {
		next = current.DeepCopy()
		mergeObject(next.Object, desired.Object)
		if jsonPatch {
			template, _, _ := unstructured.NestedMap(desired.Object, "spec", "template")
			_ = unstructured.SetNestedMap(next.Object, template, "spec", "template")
		}
	}
	next.SetName(name)
	next.SetNamespace(namespace)
	if next.GetUID() == "" {
		next.SetUID("synthetic-object-uid")
	}
	next.SetResourceVersion("2")
	if resource == "deployments" {
		replicas, found, _ := unstructured.NestedInt64(next.Object, "spec", "replicas")
		if !found {
			replicas = 1
		}
		next.Object["status"] = map[string]any{"observedGeneration": int64(1), "readyReplicas": replicas, "updatedReplicas": replicas, "availableReplicas": replicas, "replicas": replicas}
		next.SetGeneration(1)
		if f.holdRollout {
			next.Object["status"] = map[string]any{"observedGeneration": int64(0), "readyReplicas": int64(0), "updatedReplicas": int64(0), "availableReplicas": int64(0)}
		}
	}
	if r.URL.Query().Get("dryRun") != "All" {
		f.applyCount++
		if f.failSecondApply && f.applyCount == 2 {
			status(w, 422, "Invalid")
			return
		}
		f.objects[key] = next
		if f.mutated != nil {
			select {
			case f.mutated <- struct{}{}:
			default:
			}
		}
	}
	respond(next.Object)
}

func mergeObject(destination, source map[string]any) {
	for key, value := range source {
		if mapping, ok := value.(map[string]any); ok {
			if existing, ok := destination[key].(map[string]any); ok {
				mergeObject(existing, mapping)
				continue
			}
		}
		destination[key] = value
	}
}

func adminFixture(t *testing.T) (*Manager, *fakeAPI, *operations.Manager) {
	t.Helper()
	t.Setenv("PORTO_HOME", t.TempDir())
	api, server := newAPI(t)
	root := t.TempDir()
	ops := operations.New(filepath.Join(root, "operations"))
	manager := New(filepath.Join(root, "admin"), fakeConfigs{map[string]string{"porto-synthetic": server.URL}}, ops, &fakeVault{values: map[string]string{}}, nil)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := ops.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, api, ops
}

func waitAdmin(t *testing.T, ops *operations.Manager, id string) operations.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := ops.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResourceNameSuggestionsUseScopedPaginatedMetadataAndKeepDirectGetIndependent(t *testing.T) {
	manager, api, _ := adminFixture(t)
	api.listPageSize = 2
	for _, name := range []string{"synthetic-one", "synthetic-two", "synthetic-three"} {
		api.objects["/secrets/default/"+name] = object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"`+name+`","namespace":"default","uid":"synthetic-uid","annotations":{"sensitive":"synthetic-private-value"}},"data":{"token":"c3ludGhldGljLXByaXZhdGUtdmFsdWU="}}`)
	}
	api.objects["/secrets/other/hidden"] = object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"hidden","namespace":"other"}}`)
	target := Target{Context: "porto-synthetic", Namespace: "default", Version: "v1", Resource: "secrets"}
	first, err := manager.ListNames(context.Background(), target, "")
	if err != nil || len(first.Names) != 2 || first.Continue == "" {
		t.Fatalf("name suggestions did not preserve pagination: %+v %v", first, err)
	}
	second, err := manager.ListNames(context.Background(), target, first.Continue)
	if err != nil || len(second.Names) != 1 || second.Continue != "" {
		t.Fatalf("remaining name suggestions were lost: %+v %v", second, err)
	}
	encoded, err := json.Marshal([]any{first, second})
	if err != nil || bytes.Contains(encoded, []byte("synthetic-private-value")) || bytes.Contains(encoded, []byte("c3ludGhldGljLXByaXZhdGUtdmFsdWU=")) || bytes.Contains(encoded, []byte("hidden")) {
		t.Fatal("name suggestions exposed Secret payloads or another namespace")
	}
	api.mu.Lock()
	for _, request := range api.requests {
		if request.Path == "/api/v1/namespaces/default/secrets" && (!strings.Contains(request.Accept, "PartialObjectMetadata") || request.Query.Get("limit") != "200") {
			t.Errorf("name suggestions requested full resources or unbounded pages: %+v", request)
		}
	}
	api.denied["list/secrets"] = true
	api.mu.Unlock()
	if _, err := manager.ListNames(context.Background(), target, ""); err == nil {
		t.Fatal("resource name suggestions bypassed native list permission")
	}
	target.Name = "synthetic-one"
	if _, err := manager.Get(context.Background(), target, false); err != nil {
		t.Fatalf("an unavailable suggestion list blocked explicitly named inspection: %v", err)
	}
}

func TestManifestDryRunRBACConflictAndPartialApply(t *testing.T) {
	manager, api, ops := adminFixture(t)
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: synthetic-config\ndata:\n  value: synthetic\n"
	preview, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: manifest, Mode: "create"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	if api.applyCount != 0 {
		t.Fatal("server dry run mutated an object")
	}
	for _, request := range api.requests {
		if request.Method == "PATCH" && request.Query.Get("force") == "true" {
			t.Fatal("server dry run forced ownership")
		}
	}
	api.mu.Unlock()
	op, err := manager.ApplyManifest(context.Background(), "porto-synthetic", "default", preview.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	preview, err = manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: manifest, Mode: "apply"})
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.objects["/configmaps/default/synthetic-config"].SetResourceVersion("changed")
	api.mu.Unlock()
	op, err = manager.ApplyManifest(context.Background(), "porto-synthetic", "default", preview.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "failed" || !strings.Contains(result.Error, "changed") {
		t.Fatalf("concurrent edit was overwritten: %+v", result)
	}
	api.mu.Lock()
	api.denied["patch/configmaps"] = true
	api.mu.Unlock()
	if _, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: manifest}); err == nil {
		t.Fatal("read-only identity was allowed to preview a mutation")
	}
	api.mu.Lock()
	delete(api.denied, "patch/configmaps")
	api.applyCount = 0
	api.failSecondApply = true
	api.mu.Unlock()
	batch := strings.ReplaceAll(manifest, "synthetic-config", "synthetic-one") + "\n---\n" + strings.ReplaceAll(manifest, "synthetic-config", "synthetic-two")
	preview, err = manager.PreviewManifest(context.Background(), ManifestRequest{Context: "porto-synthetic", Namespace: "default", Manifest: batch, Mode: "create"})
	if err != nil {
		t.Fatal(err)
	}
	op, err = manager.ApplyManifest(context.Background(), "porto-synthetic", "default", preview.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "partial" || strings.Contains(result.Error, "synthetic-private-value") {
		t.Fatalf("partial/admission failure was reported unsafely: %+v", result)
	}
}

func TestSecretMetadataRevealAndEveryReportingPathRemainRedacted(t *testing.T) {
	manager, api, ops := adminFixture(t)
	target := Target{Context: "porto-synthetic", Namespace: "default", Version: "v1", Resource: "secrets", Name: "synthetic-secret"}
	api.objects["/secrets/default/synthetic-secret"] = object(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"synthetic-secret","namespace":"default","uid":"secret-uid","resourceVersion":"1","annotations":{"sensitive":"synthetic-private-value"}},"data":{"token":"c3ludGhldGljLXByaXZhdGUtdmFsdWU="}}`)
	list, err := manager.List(context.Background(), target, "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := manager.Get(context.Background(), target, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{list, view} {
		encoded, _ := json.Marshal(value)
		if bytes.Contains(encoded, []byte("synthetic-private-value")) || bytes.Contains(encoded, []byte("c3ludGhldGljLXByaXZhdGUtdmFsdWU=")) {
			t.Fatal("Secret value escaped metadata-only inspection")
		}
	}
	revealed, err := manager.Get(context.Background(), target, true)
	if err != nil || !revealed.Revealed || !bytes.Contains(revealed.Manifest, []byte("c3ludGhldGljLXByaXZhdGUtdmFsdWU=")) {
		t.Fatalf("authorized explicit reveal did not work: %v", err)
	}
	api.mu.Lock()
	api.denied["get/secrets"] = true
	api.mu.Unlock()
	if _, err := manager.Get(context.Background(), target, true); err == nil {
		t.Fatal("Secret reveal bypassed native RBAC")
	}
	api.mu.Lock()
	delete(api.denied, "get/secrets")
	api.mu.Unlock()
	manifest := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"synthetic-secret"},"stringData":{"token":"synthetic-private-value"}}`
	if _, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: target.Context, Namespace: target.Namespace, Manifest: manifest}); err == nil {
		t.Fatal("Secret edit was not explicit")
	}
	preview, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: target.Context, Namespace: target.Namespace, Manifest: manifest, SecretConsent: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(preview)
	if bytes.Contains(encoded, []byte("synthetic-private-value")) || bytes.Contains(encoded, []byte("c3ludGhldGljLXByaXZhdGUtdmFsdWU=")) {
		t.Fatal("Secret escaped a cached diff")
	}
	op, err := manager.ApplyManifest(context.Background(), target.Context, target.Namespace, preview.Token, true)
	if err != nil {
		t.Fatal(err)
	}
	result := waitAdmin(t, ops, op.ID)
	encoded, _ = json.Marshal(result)
	if result.Status != "succeeded" || bytes.Contains(encoded, []byte("synthetic-private-value")) {
		t.Fatalf("unsafe Secret operation record: %+v", result)
	}
}

func TestCustomServedSchemaContextNamespaceAndWorkloadActions(t *testing.T) {
	manager, api, ops := adminFixture(t)
	target := Target{Context: "porto-synthetic", Namespace: "default", Group: "fixtures.example.invalid", Version: "v1", Resource: "widgets"}
	discovery, err := manager.Discover(context.Background(), target.Context)
	if err != nil || len(discovery.Resources) == 0 {
		t.Fatalf("generic discovery failed: %+v %v", discovery, err)
	}

	schema, err := manager.Schema(context.Background(), target)
	if err != nil || schema.State != "available" || !bytes.Contains(schema.Definition, []byte("integer")) {
		t.Fatalf("actual custom schema unavailable: %+v %v", schema, err)
	}
	api.mu.Lock()
	api.schemaHash = "two"
	api.mu.Unlock()
	next, err := manager.Schema(context.Background(), target)
	if err != nil || next.Version == schema.Version {
		t.Fatal("schema version changes were hidden by a stale cache")
	}
	wrong := "apiVersion: fixtures.example.invalid/v1\nkind: Widget\nmetadata:\n  name: synthetic\n  namespace: another\nspec:\n  count: 1\n"
	if _, err := manager.PreviewManifest(context.Background(), ManifestRequest{Context: target.Context, Namespace: "default", Manifest: wrong}); err == nil {
		t.Fatal("cross-namespace manifest was silently retargeted")
	}
	if _, err := manager.List(context.Background(), Target{Namespace: "default", Version: "v1", Resource: "configmaps"}, ""); err == nil {
		t.Fatal("empty context inherited a global identity")
	}
	api.objects["apps/deployments/default/synthetic"] = object(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"synthetic","namespace":"default","uid":"deployment-uid","resourceVersion":"1","generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"readyReplicas":1,"updatedReplicas":1}}`)
	replicas := int32(2)
	action := WorkloadRequest{Target: Target{Context: target.Context, Namespace: "default", Group: "apps", Version: "v1", Resource: "deployments", Name: "synthetic", UID: "deployment-uid", ResourceVersion: "1"}, Action: "scale", Replicas: &replicas, Confirm: true}
	op, err := manager.Workload(context.Background(), action)
	if err != nil {
		t.Fatal(err)
	}
	if result := waitAdmin(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	api.mu.Lock()
	api.served = false
	api.mu.Unlock()
	if _, err := manager.Schema(context.Background(), target); err == nil {
		t.Fatal("removed served API version was silently replaced")
	}
}
