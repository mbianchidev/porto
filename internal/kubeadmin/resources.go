package kubeadmin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (m *Manager) Discover(ctx context.Context, contextName string) (Discovery, error) {
	client, err := m.client(contextName)
	if err != nil {
		return Discovery{}, err
	}
	if ctx.Err() != nil {
		return Discovery{}, ctx.Err()
	}
	_, lists, discoveryErr := client.discovery.ServerGroupsAndResources()
	result := Discovery{Resources: []Resource{}, Partial: discoveryErr != nil}
	if discoveryErr != nil {
		if len(lists) == 0 {
			return result, apiError(discoveryErr)
		}
		result.Message = "Some API groups could not be discovered. The list is partial; unavailable groups are not treated as absent."
	}
	for _, list := range lists {
		version, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			return result, invalid("The Kubernetes API returned an invalid group/version during discovery.")
		}
		for _, resource := range list.APIResources {
			if strings.Contains(resource.Name, "/") {
				continue
			}
			result.Resources = append(result.Resources, Resource{Group: version.Group, Version: version.Version, Resource: resource.Name, Kind: resource.Kind, Namespaced: resource.Namespaced, Verbs: resource.Verbs})
		}
	}
	sort.Slice(result.Resources, func(i, j int) bool {
		left, right := result.Resources[i], result.Resources[j]
		return left.Group+"/"+left.Resource+"/"+left.Version < right.Group+"/"+right.Resource+"/"+right.Version
	})
	return result, nil
}

func (m *Manager) Access(ctx context.Context, target Target) (Access, error) {
	result := Access{Target: target, Verbs: map[string]Permission{}}
	client, err := m.client(target.Context)
	if err != nil {
		return result, err
	}
	if _, err := client.resource(target); err != nil {
		return result, err
	}
	for _, verb := range []string{"get", "list", "create", "patch", "update", "delete"} {
		permission, err := client.permission(ctx, target, verb, "")
		if err != nil {
			return result, err
		}
		result.Verbs[verb] = permission
	}
	if target.Group == "apps" && (target.Resource == "deployments" || target.Resource == "statefulsets") {
		permission, err := client.permission(ctx, target, "update", "scale")
		if err != nil {
			return result, err
		}
		read, err := client.permission(ctx, target, "get", "scale")
		if err != nil {
			return result, err
		}
		permission.Allowed = permission.Allowed && read.Allowed
		if !read.Allowed {
			permission.Reason = read.Reason
		}
		result.Verbs["scale"] = permission
	}
	return result, nil
}

func (m *Manager) List(ctx context.Context, target Target, continuation string) (ObjectList, error) {
	client, err := m.client(target.Context)
	if err != nil {
		return ObjectList{}, err
	}
	resource, err := client.resource(target)
	if err != nil {
		return ObjectList{}, err
	}
	if err := client.require(ctx, target, "list", ""); err != nil {
		return ObjectList{}, err
	}
	query := metav1.ListOptions{Limit: 200, Continue: continuation}
	result := ObjectList{Resource: resource, Items: []Object{}}
	if resource.Kind == "Secret" {
		objects := client.metadata.Resource(gvr(target))
		var list *metav1.PartialObjectMetadataList
		if target.Namespace != "" {
			list, err = objects.Namespace(target.Namespace).List(ctx, query)
		} else {
			list, err = objects.List(ctx, query)
		}
		if err != nil {
			return result, apiError(err)
		}
		result.Continue = list.Continue
		for _, item := range list.Items {
			object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion(target), "kind": resource.Kind, "metadata": map[string]any{"name": item.Name, "namespace": item.Namespace, "uid": string(item.UID), "resourceVersion": item.ResourceVersion, "creationTimestamp": item.CreationTimestamp.Format(time.RFC3339), "finalizers": stringInterfaces(item.Finalizers)}}}
			result.Items = append(result.Items, objectView(target, object, false))
		}
		return result, nil
	}
	list, err := client.objects(target).List(ctx, query)
	if err != nil {
		return result, apiError(err)
	}
	result.Continue = list.GetContinue()
	for index := range list.Items {
		result.Items = append(result.Items, objectView(target, &list.Items[index], false))
	}
	return result, nil
}

func apiVersion(target Target) string {
	if target.Group == "" {
		return target.Version
	}
	return target.Group + "/" + target.Version
}

func stringInterfaces(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func (m *Manager) Get(ctx context.Context, target Target, reveal bool) (Object, error) {
	if err := validateTarget(target, true); err != nil {
		return Object{}, err
	}
	client, err := m.client(target.Context)
	if err != nil {
		return Object{}, err
	}
	resource, err := client.resource(target)
	if err != nil {
		return Object{}, err
	}
	if err := client.require(ctx, target, "get", ""); err != nil {
		return Object{}, err
	}
	if resource.Kind == "Secret" && !reveal {
		metadata, err := client.metadata.Resource(gvr(target)).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return Object{}, apiError(err)
		}
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion(target), "kind": "Secret", "metadata": map[string]any{"name": metadata.Name, "namespace": metadata.Namespace, "uid": string(metadata.UID), "resourceVersion": metadata.ResourceVersion, "finalizers": stringInterfaces(metadata.Finalizers)}}}
		return objectView(target, object, false), nil
	}
	object, err := client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return Object{}, apiError(err)
	}
	if target.UID != "" && string(object.GetUID()) != target.UID {
		return Object{}, conflict("The selected object was recreated. Refresh its identity before revealing or editing it.")
	}
	return objectView(target, object, reveal), nil
}

func objectView(target Target, object *unstructured.Unstructured, reveal bool) Object {
	target.Name, target.UID, target.ResourceVersion = object.GetName(), string(object.GetUID()), object.GetResourceVersion()
	target.Namespace = object.GetNamespace()
	visible := object.DeepCopy()
	secret := visible.GetKind() == "Secret"
	if secret && !reveal {
		visible = redactSecret(visible)
	}
	data, _ := json.MarshalIndent(visible.Object, "", "  ")
	return Object{Target: target, Kind: object.GetKind(), Manifest: data, Secret: secret, Revealed: secret && reveal}
}

func redactSecret(object *unstructured.Unstructured) *unstructured.Unstructured {
	metadata := map[string]any{"name": object.GetName(), "namespace": object.GetNamespace(), "uid": string(object.GetUID()), "resourceVersion": object.GetResourceVersion(), "finalizers": stringInterfaces(object.GetFinalizers())}
	keys := map[string]any{}
	for _, field := range []string{"data", "stringData"} {
		values, _, _ := unstructured.NestedMap(object.Object, field)
		for key := range values {
			keys[key] = "[redacted]"
		}
	}
	result := &unstructured.Unstructured{Object: map[string]any{"apiVersion": object.GetAPIVersion(), "kind": "Secret", "metadata": metadata, "data": keys}}
	if value, found, _ := unstructured.NestedString(object.Object, "type"); found {
		result.Object["type"] = value
	}
	if value, found, _ := unstructured.NestedBool(object.Object, "immutable"); found {
		result.Object["immutable"] = value
	}
	return result
}

func cleanEditable(object *unstructured.Unstructured) *unstructured.Unstructured {
	result := object.DeepCopy()
	delete(result.Object, "status")
	for _, key := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "selfLink"} {
		unstructured.RemoveNestedField(result.Object, "metadata", key)
	}
	annotations := result.GetAnnotations()
	delete(annotations, "kubectl.kubernetes.io/last-applied-configuration")
	result.SetAnnotations(annotations)
	return result
}

func (m *Manager) Schema(ctx context.Context, target Target) (Schema, error) {
	client, err := m.client(target.Context)
	if err != nil {
		return Schema{}, err
	}
	resource, err := client.resource(target)
	if err != nil {
		return Schema{}, err
	}
	if ctx.Err() != nil {
		return Schema{}, ctx.Err()
	}
	paths, err := client.discovery.OpenAPIV3().Paths()
	if err != nil {
		return Schema{State: "unavailable", Message: "The selected API did not provide OpenAPI v3. Server-side dry run remains authoritative."}, nil
	}
	key := "api/" + target.Version
	if target.Group != "" {
		key = "apis/" + target.Group + "/" + target.Version
	}
	group, ok := paths[key]
	if !ok {
		return Schema{State: "unavailable", Message: "No OpenAPI schema is published for this served resource version."}, nil
	}
	data, err := group.Schema("application/json")
	if err != nil || len(data) > 16*1024*1024 {
		return Schema{State: "unavailable", Message: "The served schema is unavailable or exceeds the 16 MiB limit."}, nil
	}
	var document struct {
		Components struct {
			Schemas map[string]json.RawMessage `json:"schemas"`
		} `json:"components"`
	}
	if json.Unmarshal(data, &document) != nil {
		return Schema{}, &Error{502, "The selected API returned malformed OpenAPI schema data."}
	}
	for _, definition := range document.Components.Schemas {
		var value struct {
			Kinds []struct{ Group, Version, Kind string } `json:"x-kubernetes-group-version-kind"`
		}
		if json.Unmarshal(definition, &value) != nil {
			continue
		}
		for _, kind := range value.Kinds {
			if kind.Group == target.Group && kind.Version == target.Version && kind.Kind == resource.Kind {
				return Schema{State: "available", Version: group.ServerRelativeURL(), Definition: definition}, nil
			}
		}
	}
	return Schema{State: "unavailable", Version: group.ServerRelativeURL(), Message: "No matching kind schema was published. Referenced types may require the API's complete OpenAPI document; server-side validation is still required."}, nil
}
