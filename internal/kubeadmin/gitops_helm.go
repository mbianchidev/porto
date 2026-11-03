package kubeadmin

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (s *session) gitOpsHelmStores(ctx context.Context, scope HelmScope, release string) ([]Target, error) {
	target := releaseStore(scope)
	if err := s.require(ctx, target, "list", ""); err != nil {
		return nil, err
	}
	options := metav1.ListOptions{Limit: 200, LabelSelector: "owner=helm,name=" + release}
	result := []Target{}
	for page := 0; page < 20; page++ {
		list, err := s.metadataObjects(target).List(ctx, options)
		if err != nil {
			return nil, apiError(err)
		}
		for _, object := range list.Items {
			prefix := "sh.helm.release.v1." + release + ".v"
			if !strings.HasPrefix(object.Name, prefix) {
				continue
			}
			if revision, err := strconv.Atoi(strings.TrimPrefix(object.Name, prefix)); err != nil || revision < 1 {
				continue
			}
			value := target
			value.Name, value.UID, value.ResourceVersion = object.Name, string(object.UID), object.ResourceVersion
			result = append(result, value)
		}
		if list.Continue == "" {
			return result, nil
		}
		options.Continue = list.Continue
	}
	return nil, &Error{503, "Native Helm release ownership inventory exceeded its bounded pagination limit."}
}

func (s *session) rejectGitOpsHelm(ctx context.Context, scope HelmScope, release string) error {
	stores, err := s.gitOpsHelmStores(ctx, scope, release)
	if err != nil {
		return err
	}
	for _, target := range stores {
		metadata, err := s.metadataObjects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return apiError(err)
		}
		if metadata.Annotations[gitOpsHandoff] != "" || metadata.Labels["helm.toolkit.fluxcd.io/name"] != "" || metadata.Annotations["argocd.argoproj.io/tracking-id"] != "" {
			return conflict("The native Helm store has a GitOps owner or staged handoff. Use that application's desired-state lifecycle, not direct Helm.")
		}
	}
	api := gitOpsAPIs["HelmRelease"]
	if _, err := s.discovery.ServerResourcesForGroupVersion(api.group + "/" + api.version); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return apiError(err)
	}
	target := Target{Context: scope.Context, Namespace: scope.Namespace, Group: api.group, Version: api.version, Resource: api.resource}
	roots, err := s.gitOpsList(ctx, target)
	if err != nil {
		return err
	}
	for index := range roots {
		root := &roots[index]
		name := gitOpsString(root, "spec", "releaseName")
		if name == "" {
			name = root.GetName()
		}
		storage := gitOpsString(root, "spec", "storageNamespace")
		if storage == "" {
			storage = root.GetNamespace()
		}
		if name == release && storage == scope.Namespace {
			return conflict("This Helm release is assigned to a GitOps HelmRelease, even if suspended or not yet reconciled. A second release controller is not permitted.")
		}
	}
	return nil
}

func (m *Manager) stageGitOpsHelmAdoption(ctx context.Context, client *session, prepared *storedGitOps, preview *GitOpsPreview, root *unstructured.Unstructured) error {
	request := prepared.Request
	if request.HelmRelease == "" || request.HelmRelease != request.Name || len(request.Inventory) > 0 {
		return invalid("Helm handoff requires the existing release's exact name as both application and release, without a separate manifest inventory. No second release is created.")
	}
	scope := HelmScope{Context: request.Context, Namespace: request.Namespace}
	if err := client.rejectGitOpsHelm(ctx, scope, request.HelmRelease); err != nil {
		return err
	}
	document, err := m.helmRelease(ctx, scope, request.HelmRelease)
	if err != nil {
		return err
	}
	if document.Info.Status != "deployed" {
		return conflict("Only a completed deployed direct Helm release can be staged for handoff. Resolve pending/failed native operations first.")
	}
	target := releaseStore(scope)
	target.Name = "sh.helm.release.v1." + request.HelmRelease + ".v" + strconv.Itoa(document.Version)
	current, err := client.gitOpsRead(ctx, target)
	if err != nil {
		return err
	}
	target.UID, target.ResourceVersion = string(current.GetUID()), current.GetResourceVersion()
	prepared.Request.Inventory = []Target{target}
	desired := cleanEditable(current)
	annotations := desired.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[gitOpsHandoff] = root.GetLabels()[gitOpsOwnerLabel]
	annotations[gitOpsHandoffTarget] = gitOpsNativeOwner(request.GitOpsScope, root, current)
	desired.SetAnnotations(annotations)
	if err := client.gitOpsPrepare(ctx, prepared, preview, target, desired, false); err != nil {
		return err
	}
	inventory, err := json.Marshal([]Target{target})
	if err != nil {
		return err
	}
	annotations = root.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["porto.dev/gitops-adoption"], annotations[gitOpsAdoptionInventory], annotations["porto.dev/gitops-adopted-release"] = "staged", string(inventory), request.HelmRelease
	root.SetAnnotations(annotations)
	_ = unstructured.SetNestedField(root.Object, false, "spec", "install", "disableTakeOwnership")
	_ = unstructured.SetNestedField(root.Object, false, "spec", "upgrade", "disableTakeOwnership")
	preview.Consequences = append(preview.Consequences, "The selected existing native Helm release is locked for handoff to one suspended HelmRelease with the same release/storage namespace. Chart upgrades, hooks and persistent data may change when you explicitly reconcile; no new parallel release is created.")
	return nil
}

func (s *session) gitOpsHelmAdoptionProof(ctx context.Context, scope GitOpsScope, root *unstructured.Unstructured) (gitOpsGuard, error) {
	release := root.GetAnnotations()["porto.dev/gitops-adopted-release"]
	if release != gitOpsString(root, "spec", "releaseName") {
		return gitOpsGuard{}, conflict("The adopted Helm release name changed. A second release was not retargeted.")
	}
	stores, err := s.gitOpsHelmStores(ctx, HelmScope{Context: scope.Context, Namespace: scope.Namespace}, release)
	if err != nil {
		return gitOpsGuard{}, err
	}
	latest, revision := Target{}, 0
	for _, target := range stores {
		value, _ := strconv.Atoi(strings.TrimPrefix(target.Name, "sh.helm.release.v1."+release+".v"))
		if value > revision {
			latest, revision = target, value
		}
	}
	if revision == 0 {
		return gitOpsGuard{}, conflict("The native Helm store has no retained release identity.")
	}
	current, err := s.gitOpsRead(ctx, latest)
	if err != nil {
		return gitOpsGuard{}, err
	}
	if !nativeGitOpsOwns(scope, root, current) {
		return gitOpsGuard{}, conflict("The latest native Helm release store is not owned by this HelmRelease. A Ready condition alone is not adoption proof.")
	}
	history, _, _ := unstructured.NestedSlice(root.Object, "status", "history")
	found := false
	for _, raw := range history {
		item, ok := raw.(map[string]any)
		if ok && item["name"] == release && item["namespace"] == scope.Namespace && item["version"] == int64(revision) && item["status"] == "deployed" {
			found = true
		}
	}
	if !found {
		return gitOpsGuard{}, conflict("HelmRelease history does not prove the same deployed native Helm release revision.")
	}
	return gitOpsGuardFor(latest, current), nil
}
