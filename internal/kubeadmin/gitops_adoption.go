package kubeadmin

import (
	"context"
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const gitOpsAdoptionInventory = "porto.dev/gitops-adoption-inventory"

func (m *Manager) stageGitOpsAdoption(ctx context.Context, s *session, prepared *storedGitOps, preview *GitOpsPreview, root *unstructured.Unstructured) error {
	request := prepared.Request
	if request.Kind == "HelmRelease" {
		return m.stageGitOpsHelmAdoption(ctx, s, prepared, preview, root)
	}
	if len(request.Inventory) == 0 || len(request.Inventory) > 50 {
		return invalid("Ownership handoff requires 1 to 50 explicit top-level resource identities with UID/resourceVersion. Direct Helm releases need the separate same-release adoption workflow.")
	}

	owner := gitOpsOwner(request.GitOpsScope, request.Kind, request.Name)
	seen := map[string]bool{}
	for _, target := range request.Inventory {
		if target.Context != request.Context || target.Namespace != request.Namespace || target.UID == "" || target.ResourceVersion == "" {
			return invalid("Every adopted resource must identify this exact context/namespace and its current UID/resourceVersion. Cluster-wide and cross-namespace handoffs are not implicit.")
		}
		if err := validateTarget(target, true); err != nil {
			return err
		}
		if seen[gitOpsTargetKey(target)] {
			return invalid("Ownership handoff contains duplicate resource identities.")
		}
		seen[gitOpsTargetKey(target)] = true
		resource, err := s.resource(target)
		if err != nil {
			return err
		}
		if resource.Kind == "Secret" && strings.HasPrefix(target.Name, "sh.helm.release.") {
			return conflict("A native Helm release store cannot be adopted as a plain manifest. Use an explicit same-release HelmRelease handoff.")
		}
		current, err := s.gitOpsRead(ctx, target)
		if err != nil {
			return err
		}
		if string(current.GetUID()) != target.UID || current.GetResourceVersion() != target.ResourceVersion {
			return conflict("An adoption target changed or was recreated. Refresh its immutable identity before staging.")
		}
		if len(current.GetOwnerReferences()) > 0 || current.GetLabels()["app.kubernetes.io/managed-by"] == "Helm" {
			return conflict("An adoption target still belongs to another controller or native Helm release. Do not create a second release/resource owner.")
		}
		previous := current.DeepCopy()
		if previous.GetAnnotations()[gitOpsHandoff] == owner {
			annotations := previous.GetAnnotations()
			delete(annotations, gitOpsHandoff)
			previous.SetAnnotations(annotations)
		}
		if err := rejectGitOpsWriter(previous); err != nil {
			return err
		}
		desired := cleanEditable(current)
		annotations := desired.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[gitOpsHandoff] = owner
		desired.SetAnnotations(annotations)
		if err := s.gitOpsPrepare(ctx, prepared, preview, target, desired, false); err != nil {
			return err
		}
	}
	data, err := json.Marshal(request.Inventory)
	if err != nil {
		return err
	}
	annotations := root.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["porto.dev/gitops-adoption"], annotations[gitOpsAdoptionInventory] = "staged", string(data)
	root.SetAnnotations(annotations)
	preview.Warnings = append(preview.Warnings, "Ownership handoff stages a suspended GitOps root and locks the exact direct-managed resource identities. Reconcile once, inspect drift and native ownership, then explicitly verify adoption. No running reconciler is displaced and no force-apply is used.")
	return nil
}

func gitOpsAdoptionTargets(root *unstructured.Unstructured) ([]Target, error) {
	var targets []Target
	if json.Unmarshal([]byte(root.GetAnnotations()[gitOpsAdoptionInventory]), &targets) != nil || len(targets) == 0 || len(targets) > 50 {
		return nil, conflict("The staged ownership inventory is unavailable or invalid. Recover the exact handoff metadata before proceeding.")
	}
	return targets, nil
}

func (s *session) guardGitOpsAdoption(ctx context.Context, prepared *storedGitOps, root *unstructured.Unstructured) error {
	targets, err := gitOpsAdoptionTargets(root)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target.Context != prepared.Request.Context || target.Namespace != prepared.Request.Namespace {
			return conflict("The staged ownership scope differs from this selected application.")
		}
		current, err := s.gitOpsRead(ctx, target)
		if err != nil {
			return err
		}
		if string(current.GetUID()) != target.UID || current.GetAnnotations()[gitOpsHandoff] != root.GetLabels()[gitOpsOwnerLabel] {
			return conflict("An adopted resource was replaced or its handoff lock changed. Inspect competing writers before reconciliation.")
		}
		prepared.Guards = append(prepared.Guards, gitOpsGuardFor(target, current))
	}
	return nil
}

func nativeGitOpsOwns(scope GitOpsScope, root, resource *unstructured.Unstructured) bool {
	if scope.Controller == "argocd" {
		expected := root.GetName() + ":" + resource.GroupVersionKind().Group + "/" + resource.GetKind() + ":" + resource.GetNamespace() + "/" + resource.GetName()
		return resource.GetAnnotations()["argocd.argoproj.io/tracking-id"] == expected || resource.GetLabels()["argocd.argoproj.io/instance"] == root.GetName()
	}
	key := "kustomize.toolkit.fluxcd.io/"
	if root.GetKind() == "HelmRelease" {
		key = "helm.toolkit.fluxcd.io/"
	}
	return resource.GetLabels()[key+"name"] == root.GetName() && resource.GetLabels()[key+"namespace"] == root.GetNamespace()
}

func (m *Manager) previewGitOpsAdoptionVerification(ctx context.Context, client *session, prepared storedGitOps, preview GitOpsPreview, root *unstructured.Unstructured) (GitOpsPreview, error) {
	guards, err := client.gitOpsAdoptionProof(ctx, prepared.Request.GitOpsScope, root)
	if err != nil {
		return preview, err
	}
	prepared.Guards = append(prepared.Guards, guards...)
	targets, err := gitOpsAdoptionTargets(root)
	if err != nil {
		return preview, err
	}
	for _, target := range targets {
		current, err := client.gitOpsRead(ctx, target)
		if err != nil {
			return preview, err
		}
		desired := cleanEditable(current)
		annotations := desired.GetAnnotations()
		delete(annotations, gitOpsHandoff)
		desired.SetAnnotations(annotations)
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, desired, false); err != nil {
			return preview, err
		}
	}
	desired := cleanEditable(root)
	annotations := desired.GetAnnotations()
	annotations["porto.dev/gitops-adoption"] = "verified"
	desired.SetAnnotations(annotations)
	if err := client.gitOpsPrepare(ctx, &prepared, &preview, gitOpsTarget(prepared.Request.GitOpsScope, root.GetKind(), root.GetName()), desired, false); err != nil {
		return preview, err
	}
	return m.storeGitOpsPreview(prepared, preview)
}

func (s *session) gitOpsAdoptionProof(ctx context.Context, scope GitOpsScope, root *unstructured.Unstructured) ([]gitOpsGuard, error) {
	if root.GetAnnotations()["porto.dev/gitops-adoption"] != "staged" {
		return nil, conflict("Only a staged ownership handoff can be verified.")
	}
	state, err := s.gitOpsState(ctx, scope, root)
	if err != nil {
		return nil, err
	}
	if state.Health != "healthy" || state.AppliedRevision == "" || state.AppliedRevision != state.DesiredRevision || state.Drift != "in-sync" {
		return nil, conflict("Native reconciliation, immutable revision and workload readiness have not verified adoption. Inspect drift and reconcile once before verification.")
	}
	targets, err := gitOpsAdoptionTargets(root)
	if err != nil {
		return nil, err
	}
	guards := []gitOpsGuard{}
	helm := root.GetKind() == "HelmRelease" && root.GetAnnotations()["porto.dev/gitops-adopted-release"] != ""
	for _, target := range targets {
		if target.Context != scope.Context || target.Namespace != scope.Namespace {
			return nil, conflict("The adoption proof is outside this exact context/namespace.")
		}
		current, err := s.gitOpsRead(ctx, target)
		if err != nil {
			return nil, err
		}
		if string(current.GetUID()) != target.UID || current.GetAnnotations()[gitOpsHandoff] != root.GetLabels()[gitOpsOwnerLabel] {
			return nil, conflict("An exact staged resource identity or ownership lock changed.")
		}
		if !helm {
			if !nativeGitOpsOwns(scope, root, current) {
				return nil, conflict("The native controller has not adopted an exact staged resource identity. A healthy revision alone is not ownership verification.")
			}
			inInventory := false
			for _, entry := range state.Inventory {
				inInventory = inInventory || entry.Group == target.Group && entry.Kind == current.GetKind() && entry.Name == target.Name && entry.Namespace == target.Namespace
			}
			if !inInventory {
				return nil, conflict("An adopted identity is absent from the native controller inventory. Labels alone do not prove ownership.")
			}
		}
		guards = append(guards, gitOpsGuardFor(target, current))
	}
	if helm {
		guard, err := s.gitOpsHelmAdoptionProof(ctx, scope, root)
		if err != nil {
			return nil, err
		}
		guards = append(guards, guard)
	}
	return guards, nil
}
