package kubeadmin

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func (m *Manager) previewGitOpsDelete(ctx context.Context, client *session, prepared storedGitOps, preview GitOpsPreview, root *unstructured.Unstructured) (GitOpsPreview, error) {
	request, scope := prepared.Request, prepared.Request.GitOpsScope
	if request.DeletePolicy != "orphan" && request.DeletePolicy != "prune" {
		return preview, invalid("Choose explicit orphan/preserve or controller-native prune/uninstall deletion.")
	}
	if request.DeletePolicy == "prune" {
		preview.Consequences = append(preview.Consequences, "Controller-native pruning/uninstall can remove workloads, hooks and persistent data. Namespace, CRD and storage consequences require this additional acknowledgement; reverting a source cannot restore them.")
		inventory := gitOpsInventory(root)
		if root.GetKind() == "HelmRelease" {
			release := gitOpsString(root, "spec", "releaseName")
			document, err := m.helmRelease(ctx, HelmScope{Context: scope.Context, Namespace: scope.Namespace}, release)
			if err != nil {
				return preview, err
			}
			objects, err := decodeDocuments(releaseManifests(document), 16*1024*1024, 1000)
			if err != nil {
				return preview, err
			}
			for _, object := range objects {
				inventory = append(inventory, GitOpsResource{Group: object.GroupVersionKind().Group, Version: object.GroupVersionKind().Version, Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()})
			}
		}
		if len(inventory) == 0 {
			preview.Consequences = append(preview.Consequences, "The controller has no complete rendered inventory yet. Treat namespace, CRD and persistent-storage impact as unknown, not empty.")
		}
		for _, entry := range inventory {
			if slices.Contains([]string{"Namespace", "CustomResourceDefinition", "PersistentVolume", "PersistentVolumeClaim"}, entry.Kind) {
				preview.Consequences = append(preview.Consequences, fmt.Sprintf("%s %s/%s can remove dependent resources or persistent data; inspect native reclaim, keep and finalizer policies.", entry.Kind, entry.Namespace, entry.Name))
			}
		}
	}
	desired := cleanEditable(root)
	setGitOpsSuspended(desired, scope.Controller, true)
	if root.GetKind() == "Kustomization" {
		_ = unstructured.SetNestedField(desired.Object, request.DeletePolicy == "prune", "spec", "prune")
		policy := "Orphan"
		if request.DeletePolicy == "prune" {
			policy = "Delete"
		}
		_ = unstructured.SetNestedField(desired.Object, policy, "spec", "deletionPolicy")
	} else if root.GetKind() == "HelmRelease" {
		if request.DeletePolicy == "orphan" {
			desired.SetFinalizers(slices.DeleteFunc(desired.GetFinalizers(), func(value string) bool { return value == "finalizers.fluxcd.io" }))
			preview.Consequences = append(preview.Consequences, "Orphaning a suspended HelmRelease explicitly removes only its Flux finalizer. The native Helm release and workloads remain; unrelated finalizers are preserved.")
		}
	} else {
		_ = unstructured.SetNestedField(desired.Object, false, "spec", "syncPolicy", "automated", "enabled")
		finalizers := slices.DeleteFunc(desired.GetFinalizers(), func(value string) bool {
			return value == "resources-finalizer.argocd.argoproj.io" || value == "resources-finalizer.argocd.argoproj.io/background"
		})
		if request.DeletePolicy == "prune" {
			setGitOpsSuspended(desired, scope.Controller, false)
			finalizers = append(finalizers, "resources-finalizer.argocd.argoproj.io")
		}
		desired.SetFinalizers(finalizers)
	}
	target := gitOpsTarget(scope, root.GetKind(), root.GetName())
	if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, desired, false); err != nil {
		return preview, err
	}
	if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, nil, true); err != nil {
		return preview, err
	}
	if scope.Controller == "flux" {
		sourceKind, sourceName := gitOpsString(root, "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "sourceRef", "name")
		if root.GetKind() == "HelmRelease" {
			sourceKind, sourceName = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "chart", "spec", "sourceRef", "name")
		}
		if sourceKind != "GitRepository" && sourceKind != "HelmRepository" {
			return preview, invalid("An unsupported native source is not deleted implicitly.")
		}
		if err := client.gitOpsExclusiveSource(ctx, scope, sourceKind, sourceName, root.GetKind(), root.GetName()); err != nil {
			return preview, err
		}
		sourceTarget := gitOpsTarget(scope, sourceKind, sourceName)
		source, err := client.gitOpsRead(ctx, sourceTarget)
		if err != nil {
			return preview, err
		}
		if source.GetLabels()[gitOpsOwnerLabel] != root.GetLabels()[gitOpsOwnerLabel] {
			return preview, conflict("The selected source belongs to another application. It was not included in deletion.")
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, sourceTarget, nil, true); err != nil {
			return preview, err
		}
	}
	secretTarget := Target{Context: scope.Context, Namespace: target.Namespace, Version: "v1", Resource: "secrets", Name: root.GetName() + "-access"}
	if err := client.require(ctx, secretTarget, "get", ""); err != nil {
		return preview, err
	}
	secret, err := client.objects(secretTarget).Get(ctx, secretTarget.Name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return preview, apiError(err)
	}
	if err == nil {
		if secret.GetLabels()[gitOpsOwnerLabel] != root.GetLabels()[gitOpsOwnerLabel] {
			return preview, conflict("A same-named source Secret belongs to another owner and was not deleted.")
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, secretTarget, nil, true); err != nil {
			return preview, err
		}
	}
	if root.GetAnnotations()["porto.dev/gitops-adoption"] == "staged" {
		targets, err := gitOpsAdoptionTargets(root)
		if err != nil {
			return preview, err
		}
		for _, resourceTarget := range targets {
			current, err := client.gitOpsRead(ctx, resourceTarget)
			if err != nil {
				return preview, err
			}
			if string(current.GetUID()) != resourceTarget.UID || current.GetAnnotations()[gitOpsHandoff] != root.GetLabels()[gitOpsOwnerLabel] {
				return preview, conflict("The staged ownership inventory changed. Pending handoff locks were not removed from unrelated resources.")
			}
			updated := cleanEditable(current)
			annotations := updated.GetAnnotations()
			delete(annotations, gitOpsHandoff)
			updated.SetAnnotations(annotations)
			if err := client.gitOpsPrepare(ctx, &prepared, &preview, resourceTarget, updated, false); err != nil {
				return preview, err
			}
		}
	}
	preview.Warnings = append(preview.Warnings, "Deletion targets only the selected GitOps root and its exclusively owned source/credential resources. Controller installations, namespaces and local source registrations are not removed. Native finalizer failures remain partial operations, never forced success.")
	return m.storeGitOpsPreview(prepared, preview)
}

func (s *session) observeGitOpsDeletion(parent context.Context, target Target, uid types.UID) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		current, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return apiError(err)
		}
		if current.GetUID() != uid {
			return conflict("The deleted name was recreated. Its replacement was not retargeted or removed.")
		}
		select {
		case <-ctx.Done():
			return operations.Partial("Native deletion remains pending or its observation was interrupted. Inspect exact UID and finalizers; no finalizer was bypassed.")
		case <-ticker.C:
		}
	}
}
