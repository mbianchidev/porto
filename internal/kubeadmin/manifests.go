package kubeadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const fieldManager = "porto-admin"

func decodeManifests(text string) ([]*unstructured.Unstructured, error) {
	return decodeDocuments(text, 2*1024*1024, 50)
}

func decodeDocuments(text string, maxBytes, maxObjects int) ([]*unstructured.Unstructured, error) {
	if len(text) == 0 || len(text) > maxBytes {
		return nil, invalid("Manifest input is empty or exceeds this operation's document limit.")
	}
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
	result := []*unstructured.Unstructured{}
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, invalid("Manifest YAML/JSON could not be decoded. Check indentation and document syntax; input values were not logged.")
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.IsList() {
			list, err := object.ToList()
			if err != nil {
				return nil, invalid("Manifest List items could not be decoded.")
			}
			for index := range list.Items {
				result = append(result, list.Items[index].DeepCopy())
			}
		} else {
			result = append(result, object.DeepCopy())
		}
		if len(result) > maxObjects {
			return nil, invalid("Manifest resource count exceeds this operation's bounded inventory.")
		}
	}
	if len(result) == 0 {
		return nil, invalid("The manifest has no resources.")
	}
	return result, nil
}

func (s *session) targetForObject(contextName, namespace string, object *unstructured.Unstructured) (Target, Resource, error) {
	version, err := schema.ParseGroupVersion(object.GetAPIVersion())
	if err != nil || object.GetKind() == "" || object.GetName() == "" {
		return Target{}, Resource{}, invalid("Each manifest requires apiVersion, kind and metadata.name; generated-name requests are not supported.")
	}
	list, err := s.discovery.ServerResourcesForGroupVersion(version.String())
	if err != nil {
		return Target{}, Resource{}, apiError(err)
	}
	for _, resource := range list.APIResources {
		if resource.Kind != object.GetKind() || strings.Contains(resource.Name, "/") {
			continue
		}
		target := Target{Context: contextName, Namespace: namespace, Group: version.Group, Version: version.Version, Resource: resource.Name, Name: object.GetName()}
		if resource.Namespaced {
			if namespace == "" {
				return target, Resource{}, invalid("Select a namespace explicitly before applying namespaced resources.")
			}
			if declared := object.GetNamespace(); declared != "" && declared != namespace {
				return target, Resource{}, invalid("A manifest namespace differs from the selected namespace. Cross-namespace application is not implicit.")
			}
			object.SetNamespace(namespace)
		} else {
			if object.GetNamespace() != "" {
				return target, Resource{}, invalid("A cluster-scoped resource cannot carry a namespace.")
			}
			target.Namespace = ""
		}
		if err := validateTarget(target, true); err != nil {
			return target, Resource{}, err
		}
		return target, Resource{Group: target.Group, Version: target.Version, Resource: resource.Name, Kind: resource.Kind, Namespaced: resource.Namespaced, Verbs: resource.Verbs}, nil
	}
	return Target{}, Resource{}, &Error{404, "The manifest kind is not served in its requested API version. Refresh discovery/schema instead of substituting a version."}
}

func (m *Manager) PreviewManifest(ctx context.Context, request ManifestRequest) (Preview, error) {
	if request.Mode == "" {
		request.Mode = "apply"
	}
	if request.Mode != "apply" && request.Mode != "create" {
		return Preview{}, invalid("Manifest mode must be create or conflict-aware apply.")
	}
	objects, err := decodeManifests(request.Manifest)
	if err != nil {
		return Preview{}, err
	}
	client, err := m.client(request.Context)
	if err != nil {
		return Preview{}, err
	}
	prepared := storedManifest{Context: request.Context, Namespace: request.Namespace, Mode: request.Mode, Identity: client.identity, Objects: []storedObject{}}
	preview := Preview{Context: request.Context, Namespace: request.Namespace, Differences: []Difference{}, Warnings: []string{"Server-side dry run validates admission/schema but is not a transaction. Apply acceptance does not prove workload readiness. Existing UID/resourceVersion and field ownership are rechecked; force-overwrite is never used."}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	seen := map[string]bool{}
	for _, object := range objects {
		target, resource, err := client.targetForObject(request.Context, request.Namespace, object)
		if err != nil {
			return Preview{}, err
		}
		if resource.Kind == "Secret" && !request.SecretConsent {
			return Preview{}, invalid("Explicit Secret create/edit consent is required. Secret values are hidden from every diff and operation record.")
		}
		identity := target.Group + "/" + target.Resource + "/" + target.Namespace + "/" + target.Name
		if seen[identity] {
			return Preview{}, invalid("One preview cannot contain duplicate identities for the same object.")
		}
		seen[identity] = true
		var before *unstructured.Unstructured
		exists := false
		if request.Mode == "apply" {
			if err := client.require(ctx, target, "get", ""); err != nil {
				return Preview{}, err
			}
			before, err = client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return Preview{}, apiError(err)
			}
			exists = err == nil
			if err := client.require(ctx, target, "patch", ""); err != nil {
				return Preview{}, err
			}
			if !exists {
				if err := client.require(ctx, target, "create", ""); err != nil {
					return Preview{}, err
				}
			}
		} else {
			if err := client.require(ctx, target, "create", ""); err != nil {
				return Preview{}, err
			}
		}
		desired := cleanEditable(object)
		managers := []string{}
		beforeJSON := json.RawMessage("null")
		if exists {
			if expected := object.GetResourceVersion(); expected != "" && expected != before.GetResourceVersion() {
				return Preview{}, conflict("Manifest resourceVersion no longer matches the selected object. Refresh before editing.")
			}
			if expected := object.GetUID(); expected != "" && expected != before.GetUID() {
				return Preview{}, conflict("The selected object was recreated. Refresh before editing.")
			}
			target.UID, target.ResourceVersion = string(before.GetUID()), before.GetResourceVersion()
			desired.SetResourceVersion(before.GetResourceVersion())
			beforeJSON = publicObject(before)
			for _, owner := range before.GetManagedFields() {
				if owner.Manager != "" {
					managers = append(managers, owner.Manager)
				}
			}
			if before.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" {
				preview.Warnings = append(preview.Warnings, "A selected object is Helm-owned. Prefer updating its Helm release; direct edits may be reconciled or rejected by ownership conflicts.")
			}
		}
		data, err := json.Marshal(desired.Object)
		if err != nil {
			return Preview{}, invalid("Manifest could not be serialized.")
		}
		var after *unstructured.Unstructured
		if request.Mode == "create" {
			after, err = client.objects(target).Create(ctx, desired, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict})
		} else {
			force := false
			after, err = client.objects(target).Patch(ctx, target.Name, types.ApplyPatchType, data, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: fieldManager, Force: &force, FieldValidation: metav1.FieldValidationStrict})
		}
		if err != nil {
			return Preview{}, apiError(err)
		}
		preview.Differences = append(preview.Differences, Difference{Target: target, Kind: resource.Kind, Before: beforeJSON, After: publicObject(after), Managers: managers})
		prepared.Objects = append(prepared.Objects, storedObject{Target: target, Data: data, Exists: exists})
	}
	if client.warnings.count.Load() > 0 {
		preview.Warnings = append(preview.Warnings, "Kubernetes returned schema/admission warnings. Their values were withheld from cached previews; review the served schema and selected resource.")
	}
	token, err := randomID()
	if err != nil {
		return Preview{}, err
	}
	preview.Token = token
	encoded, err := json.Marshal(prepared)
	if err != nil {
		return Preview{}, err
	}
	account := "manifest-" + token
	if err := m.vault.Set(account, encoded); err != nil {
		return Preview{}, &Error{503, "Encrypted preview storage is unavailable. Unlock the system credential store and check Porto state storage; no plaintext preview was retained."}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, current := range m.previews {
		expiry, _ := time.Parse(time.RFC3339Nano, current.Preview.ExpiresAt)
		if time.Now().After(expiry) {
			if err := m.vault.Delete(current.Account); err != nil {
				return Preview{}, err
			}
			delete(m.previews, id)
		}
	}
	if len(m.previews) >= 16 {
		if err := m.vault.Delete(account); err != nil {
			return Preview{}, err
		}
		return Preview{}, conflict("Too many pending manifest previews. Wait for expiration before creating another.")
	}
	m.previews[token] = pendingManifest{Preview: preview, Account: account}
	return preview, nil
}

func publicObject(object *unstructured.Unstructured) json.RawMessage {
	if object == nil {
		return json.RawMessage("null")
	}
	visible := object.DeepCopy()
	if visible.GetKind() == "Secret" {
		visible = redactSecret(visible)
	}
	unstructured.RemoveNestedField(visible.Object, "metadata", "managedFields")
	annotations := visible.GetAnnotations()
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	visible.SetAnnotations(annotations)
	data, _ := json.MarshalIndent(visible.Object, "", "  ")
	return data
}

func (m *Manager) ApplyManifest(ctx context.Context, contextName, namespace, token string, confirm bool) (operations.Operation, error) {
	if !confirm {
		return operations.Operation{}, invalid("Confirm the exact manifest preview before applying it.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.previews[token]
	if !ok || pending.Preview.Context != contextName || pending.Preview.Namespace != namespace {
		return operations.Operation{}, conflict("A matching explicit context/namespace preview is required.")
	}
	expiry, err := time.Parse(time.RFC3339Nano, pending.Preview.ExpiresAt)
	if err != nil || time.Now().After(expiry) {
		return operations.Operation{}, conflict("Manifest preview expired. Preview current state again.")
	}
	data, err := m.vault.Get(pending.Account)
	if err != nil {
		return operations.Operation{}, &Error{503, "The encrypted manifest preview is unavailable. No resource was applied."}
	}
	var prepared storedManifest
	if err := json.Unmarshal(data, &prepared); err != nil {
		return operations.Operation{}, invalid("Stored manifest preview is invalid.")
	}
	op, err := m.operations.Start(ctx, contextKey(contextName), "kubernetes."+prepared.Mode, func(ctx context.Context, progress func(string) error) error {
		m.mu.Lock()
		m.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := m.client(contextName)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if client.identity != prepared.Identity {
			return operations.Failure("The selected kubeconfig/identity changed after preview. Preview again; no context was substituted.")
		}
		for _, object := range prepared.Objects {
			if _, err := client.resource(object.Target); err != nil {
				return operations.Failure(publicError(err))
			}
			verb := "patch"
			if prepared.Mode == "create" {
				verb = "create"
			}
			if err := client.require(ctx, object.Target, verb, ""); err != nil {
				return operations.Failure(publicError(err))
			}
			if prepared.Mode == "apply" {
				current, err := client.objects(object.Target).Get(ctx, object.Target.Name, metav1.GetOptions{})
				if object.Exists {
					if err != nil {
						return operations.Failure(publicError(apiError(err)))
					}
					if string(current.GetUID()) != object.Target.UID || current.GetResourceVersion() != object.Target.ResourceVersion {
						return operations.Failure("An object changed or was recreated after preview. Refresh and compare ownership; nothing was applied.")
					}
				} else if err == nil || !apierrors.IsNotFound(err) {
					return operations.Failure("An object appeared or became unavailable after preview. Refresh before applying; nothing was applied.")
				}
			}
		}
		applied := 0
		for index, object := range prepared.Objects {
			if err := progress(fmt.Sprintf("applying resource %d of %d in the selected context/namespace", index+1, len(prepared.Objects))); err != nil {
				return err
			}
			var desired unstructured.Unstructured
			if err := json.Unmarshal(object.Data, &desired); err != nil {
				return operations.Failure("Encrypted manifest data could not be decoded.")
			}
			if prepared.Mode == "create" {
				_, err = client.objects(object.Target).Create(ctx, &desired, metav1.CreateOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict})
			} else {
				force := false
				_, err = client.objects(object.Target).Patch(ctx, object.Target.Name, types.ApplyPatchType, object.Data, metav1.PatchOptions{FieldManager: fieldManager, Force: &force, FieldValidation: metav1.FieldValidationStrict})
			}
			if err != nil {
				return mutationError(err, applied)
			}
			applied++
		}
		return progress("all selected resources accepted; inspect workload conditions for readiness")
	})
	if err != nil {
		return op, err
	}
	delete(m.previews, token)
	if err := m.vault.Delete(pending.Account); err != nil {
		cancelErr := m.operations.Cancel(op.ID)
		return op, errors.Join(err, cancelErr)
	}
	return op, nil
}
