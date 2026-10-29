package kubeadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func actionAllowed(target Target, action string) bool {
	if action == "delete" {
		return true
	}
	if target.Group == "apps" {
		switch target.Resource {
		case "deployments":
			return slices.Contains([]string{"scale", "restart", "pause", "resume", "rollback"}, action)
		case "statefulsets":
			return slices.Contains([]string{"scale", "restart", "rollback"}, action)
		case "daemonsets":
			return action == "restart" || action == "rollback"
		}
	}
	if target.Group == "batch" {
		if target.Resource == "jobs" {
			return slices.Contains([]string{"scale", "suspend", "resume"}, action)
		}
		if target.Resource == "cronjobs" {
			return action == "suspend" || action == "resume"
		}
	}
	return false
}

func (m *Manager) Workload(ctx context.Context, request WorkloadRequest) (operations.Operation, error) {
	if err := validateTarget(request.Target, true); err != nil {
		return operations.Operation{}, err
	}
	if !request.Confirm || request.UID == "" || request.ResourceVersion == "" {
		return operations.Operation{}, invalid("Confirm the selected context/namespace and exact UID/resourceVersion before a resource action.")
	}
	if !actionAllowed(request.Target, request.Action) {
		return operations.Operation{}, invalid("This action is not supported by the selected workload/resource type.")
	}
	if request.Action == "scale" && (request.Replicas == nil || *request.Replicas < 0) {
		return operations.Operation{}, invalid("Scaling requires a nonnegative native Kubernetes replica/parallelism count.")
	}
	if request.Action == "rollback" && request.Revision < 1 {
		return operations.Operation{}, invalid("Select an explicit recorded Kubernetes controller revision.")
	}
	if request.Action == "delete" && slices.Contains([]string{"persistentvolumes", "persistentvolumeclaims", "namespaces", "customresourcedefinitions"}, request.Resource) && !request.AcknowledgeStorage {
		return operations.Operation{}, invalid("Acknowledge storage/custom-resource deletion consequences explicitly. Namespace/CRD deletion can remove dependent data and resources.")
	}
	client, err := m.client(request.Context)
	if err != nil {
		return operations.Operation{}, err
	}
	if _, err := client.resource(request.Target); err != nil {
		return operations.Operation{}, err
	}
	if err := client.require(ctx, request.Target, "get", ""); err != nil {
		return operations.Operation{}, err
	}
	verb, subresource := "patch", ""
	if request.Action == "delete" {
		verb = "delete"
	}
	if request.Action == "scale" && request.Group == "apps" {
		verb, subresource = "update", "scale"
	}
	if err := client.require(ctx, request.Target, verb, subresource); err != nil {
		return operations.Operation{}, err
	}
	identity := client.identity
	return m.operations.Start(ctx, contextKey(request.Context), "kubernetes."+request.Action, func(ctx context.Context, progress func(string) error) error {
		client, err := m.client(request.Context)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if client.identity != identity {
			return operations.Failure("The selected kubeconfig identity changed. Refresh the resource before retrying.")
		}
		if _, err := client.resource(request.Target); err != nil {
			return operations.Failure(publicError(err))
		}
		if err := client.require(ctx, request.Target, verb, subresource); err != nil {
			return operations.Failure(publicError(err))
		}
		object, err := client.objects(request.Target).Get(ctx, request.Name, metav1.GetOptions{})
		if err != nil {
			return operations.Failure(publicError(apiError(err)))
		}
		if string(object.GetUID()) != request.UID || object.GetResourceVersion() != request.ResourceVersion {
			return operations.Failure("The selected resource changed or was recreated. Refresh its UID/resourceVersion; no action was applied.")
		}
		if err := progress("submitting the explicitly selected resource action"); err != nil {
			return err
		}
		if request.Action == "delete" {
			uid, version := types.UID(request.UID), request.ResourceVersion
			propagation := metav1.DeletePropagationForeground
			err := client.objects(request.Target).Delete(ctx, request.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}, PropagationPolicy: &propagation})
			if err != nil {
				return mutationError(err, 0)
			}
			current, err := client.objects(request.Target).Get(ctx, request.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return operations.Partial("Deletion was accepted but could not be observed. Inspect the selected object's deletion timestamp and finalizers; no force deletion was used.")
			}
			if len(current.GetFinalizers()) > 0 || current.GetDeletionTimestamp() != nil {
				return operations.Partial("Deletion is pending foreground dependents or finalizers. Data consequences follow Kubernetes policies; finalizers were not removed or bypassed.")
			}
			return operations.Partial("Deletion was submitted but is not yet complete. Inspect the selected resource before retrying.")
		}
		if request.Action == "scale" && request.Group == "apps" {
			if err := client.require(ctx, request.Target, "get", "scale"); err != nil {
				return operations.Failure(publicError(err))
			}
			scale, err := client.objects(request.Target).Get(ctx, request.Name, metav1.GetOptions{}, "scale")
			if err != nil {
				return operations.Failure(publicError(apiError(err)))
			}
			if scale.GetResourceVersion() != request.ResourceVersion {
				return operations.Failure("The workload scale changed concurrently. Refresh before scaling.")
			}
			if err := unstructured.SetNestedField(scale.Object, int64(*request.Replicas), "spec", "replicas"); err != nil {
				return operations.Failure("Scale data could not be constructed.")
			}
			if _, err := client.objects(request.Target).Update(ctx, scale, metav1.UpdateOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict}, "scale"); err != nil {
				return mutationError(err, 0)
			}
			return client.waitRollout(ctx, request.Target, progress)
		}
		patch := map[string]any{"metadata": map[string]any{"resourceVersion": request.ResourceVersion}}
		switch request.Action {
		case "restart":
			patch["spec"] = map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": time.Now().UTC().Format(time.RFC3339Nano)}}}}
		case "pause":
			patch["spec"] = map[string]any{"paused": true}
		case "suspend":
			patch["spec"] = map[string]any{"suspend": true}
		case "resume":
			if request.Group == "apps" {
				patch["spec"] = map[string]any{"paused": false}
			} else {
				patch["spec"] = map[string]any{"suspend": false}
			}
		case "scale":
			patch["spec"] = map[string]any{"parallelism": int64(*request.Replicas)}
		case "rollback":
			template, err := client.rollbackTemplate(ctx, request.Target, object, request.Revision)
			if err != nil {
				return operations.Failure(publicError(err))
			}
			data, err := json.Marshal([]map[string]any{
				{"op": "test", "path": "/metadata/uid", "value": request.UID},
				{"op": "test", "path": "/metadata/resourceVersion", "value": request.ResourceVersion},
				{"op": "replace", "path": "/spec/template", "value": template},
			})
			if err != nil {
				return operations.Failure("The retained pod template could not be encoded.")
			}
			if _, err := client.objects(request.Target).Patch(ctx, request.Name, types.JSONPatchType, data, metav1.PatchOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict}); err != nil {
				return mutationError(err, 0)
			}
			return client.waitRollout(ctx, request.Target, progress)
		}
		data, err := json.Marshal(patch)
		if err != nil {
			return operations.Failure("The selected workload patch could not be encoded.")
		}
		if _, err := client.objects(request.Target).Patch(ctx, request.Name, types.MergePatchType, data, metav1.PatchOptions{FieldManager: fieldManager, FieldValidation: metav1.FieldValidationStrict}); err != nil {
			return mutationError(err, 0)
		}
		if request.Action == "restart" {
			return client.waitRollout(ctx, request.Target, progress)
		}
		return progress("resource action accepted; inspect current conditions and scheduling state")
	})
}

func (s *session) waitRollout(parent context.Context, target Target, progress func(string) error) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		object, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return operations.Partial("Workload mutation was accepted, but rollout observation failed. Inspect current conditions; the mutation was not automatically undone.")
		}
		if string(object.GetUID()) != target.UID {
			return operations.Partial("Workload identity changed while observing rollout. The replacement was not retargeted.")
		}
		paused, _, _ := unstructured.NestedBool(object.Object, "spec", "paused")
		strategy, _, _ := unstructured.NestedString(object.Object, "spec", "updateStrategy", "type")
		if paused || strategy == "OnDelete" {
			return operations.Partial("Workload update was accepted, but its paused/OnDelete policy requires explicit operator progression. No pods were force-replaced.")
		}
		conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
		for _, raw := range conditions {
			condition, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if condition["type"] == "Progressing" && condition["status"] == "False" && condition["reason"] == "ProgressDeadlineExceeded" {
				return operations.Partial("Kubernetes reported ProgressDeadlineExceeded. Inspect workload conditions and pod events before choosing a supported rollback.")
			}
		}
		desired := int64(1)
		if replicas, found, _ := unstructured.NestedInt64(object.Object, "spec", "replicas"); found {
			desired = replicas
		}
		ready, _, _ := unstructured.NestedInt64(object.Object, "status", "readyReplicas")
		updated, _, _ := unstructured.NestedInt64(object.Object, "status", "updatedReplicas")
		if target.Resource == "daemonsets" {
			desired, _, _ = unstructured.NestedInt64(object.Object, "status", "desiredNumberScheduled")
			ready, _, _ = unstructured.NestedInt64(object.Object, "status", "numberReady")
			updated, _, _ = unstructured.NestedInt64(object.Object, "status", "updatedNumberScheduled")
		}
		observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
		complete := observed >= object.GetGeneration() && ready == desired && updated == desired
		if target.Resource == "deployments" {
			total, _, _ := unstructured.NestedInt64(object.Object, "status", "replicas")
			complete = complete && total == desired
		}
		if complete {
			return progress("controller rollout is ready; this does not prove application-level health")
		}
		if err := progress(fmt.Sprintf("observing rollout: %d/%d ready, %d/%d updated", ready, desired, updated, desired)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return operations.Partial("Workload mutation was accepted, but observation was canceled or timed out. Inspect current rollout state; no rollback is implied.")
		case <-ticker.C:
		}
	}
}

func (s *session) controllerRevisions(ctx context.Context, target Target, owner *unstructured.Unstructured) ([]unstructured.Unstructured, error) {
	resource := "controllerrevisions"
	if target.Resource == "deployments" {
		resource = "replicasets"
	}
	history := Target{Context: target.Context, Namespace: target.Namespace, Group: "apps", Version: "v1", Resource: resource}
	if err := s.require(ctx, history, "list", ""); err != nil {
		return nil, err
	}
	options := metav1.ListOptions{Limit: 500}
	result := []unstructured.Unstructured{}
	for page := 0; page < 20; page++ {
		list, err := s.objects(history).List(ctx, options)
		if err != nil {
			return nil, apiError(err)
		}
		for _, item := range list.Items {
			for _, reference := range item.GetOwnerReferences() {
				if reference.UID == owner.GetUID() && reference.Controller != nil && *reference.Controller {
					result = append(result, item)
					break
				}
			}
		}
		if list.GetContinue() == "" {
			return result, nil
		}
		options.Continue = list.GetContinue()
	}
	return nil, &Error{503, "Controller history exceeded its bounded inventory; narrow or clean up the selected namespace before retrying."}
}

func controllerRevision(object *unstructured.Unstructured, resource string) int64 {
	if resource == "deployments" {
		version, _ := strconv.ParseInt(object.GetAnnotations()["deployment.kubernetes.io/revision"], 10, 64)
		return version
	}
	version, _, _ := unstructured.NestedInt64(object.Object, "revision")
	return version
}

func (s *session) rollbackTemplate(ctx context.Context, target Target, current *unstructured.Unstructured, revision int64) (map[string]any, error) {
	items, err := s.controllerRevisions(ctx, target, current)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if controllerRevision(&item, target.Resource) != revision {
			continue
		}
		path := []string{"data", "spec", "template"}
		if target.Resource == "deployments" {
			path = []string{"spec", "template"}
		}
		template, found, err := unstructured.NestedMap(item.Object, path...)
		if err != nil || !found {
			return nil, invalid("The selected controller revision has no restorable pod template.")
		}
		unstructured.RemoveNestedField(template, "metadata", "labels", "pod-template-hash")
		delete(template, "$patch")
		return template, nil
	}
	return nil, &Error{404, "The selected revision is not retained by this exact controller UID. No similarly named workload was used."}
}

func (m *Manager) RolloutHistory(ctx context.Context, target Target) ([]RolloutRevision, error) {
	if !actionAllowed(target, "rollback") {
		return nil, invalid("Controller rollback history is supported for Deployments, StatefulSets and DaemonSets.")
	}
	client, err := m.client(target.Context)
	if err != nil {
		return nil, err
	}
	if _, err := client.resource(target); err != nil {
		return nil, err
	}
	if err := client.require(ctx, target, "get", ""); err != nil {
		return nil, err
	}
	object, err := client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return nil, apiError(err)
	}
	if target.UID != "" && string(object.GetUID()) != target.UID {
		return nil, conflict("The selected workload was recreated. Refresh before reading its history.")
	}
	items, err := client.controllerRevisions(ctx, target, object)
	if err != nil {
		return nil, err
	}
	result := []RolloutRevision{}
	for _, item := range items {
		version := controllerRevision(&item, target.Resource)
		if version < 1 {
			continue
		}
		path := []string{"data", "spec", "template", "spec", "containers"}
		if target.Resource == "deployments" {
			path = []string{"spec", "template", "spec", "containers"}
		}
		containers, _, _ := unstructured.NestedSlice(item.Object, path...)
		images := []string{}
		for _, value := range containers {
			container, ok := value.(map[string]any)
			if !ok {
				continue
			}
			image, ok := container["image"].(string)
			if !ok {
				continue
			}
			if _, err := reference.ParseAnyReference(image); err == nil {
				images = append(images, image)
			} else {
				images = append(images, "[invalid image reference]")
			}
		}
		result = append(result, RolloutRevision{Revision: version, CreatedAt: item.GetCreationTimestamp().Format(time.RFC3339), Images: images})
	}
	slices.SortFunc(result, func(a, b RolloutRevision) int {
		if a.Revision > b.Revision {
			return -1
		}
		if a.Revision < b.Revision {
			return 1
		}
		return 0
	})
	return result, nil
}
