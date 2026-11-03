package kubeadmin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const gitOpsOwnershipPolicy = "porto-gitops-ownership"

func gitOpsOwnershipObjects(scope GitOpsScope) []*unstructured.Unstructured {
	shared := GitOpsScope{Context: scope.Context, Controller: "shared"}
	owner := gitOpsOwner(shared, "OwnershipPolicy", gitOpsOwnershipPolicy)
	metadata := func(name string) map[string]any {
		return map[string]any{"name": name, "labels": map[string]any{gitOpsOwnerLabel: owner, gitOpsControllerLabel: "shared"}, "annotations": map[string]any{gitOpsInstallation: "namespace-ownership-v1"}}
	}
	variables := []any{}
	for _, entry := range []struct{ name, object, field string }{{"nl", "object", "labels"}, {"ol", "oldObject", "labels"}, {"na", "object", "annotations"}, {"oa", "oldObject", "annotations"}} {
		variables = append(variables, map[string]any{"name": entry.name, "expression": fmt.Sprintf("%s != null && has(%s.metadata.%s) ? %s.metadata.%s : {}", entry.object, entry.object, entry.field, entry.object, entry.field)})
	}
	ownerExpression := func(labels, annotations string) string {
		value := func(variable, key string) string {
			return fmt.Sprintf("(%q in variables.%s ? variables.%s[%q] : '')", key, variable, variable, key)
		}
		flux := value(labels, "kustomize.toolkit.fluxcd.io/name")
		helm := value(labels, "helm.toolkit.fluxcd.io/name")
		argo := value(annotations, "argocd.argoproj.io/tracking-id")
		argoLabel := value(labels, "argocd.argoproj.io/instance")
		release := value(annotations, "meta.helm.sh/release-name")
		entries := []struct{ condition, identity string }{
			{flux + " != ''", "'flux/' + " + value(labels, "kustomize.toolkit.fluxcd.io/namespace") + " + '/' + " + flux},
			{helm + " != '' && (" + release + " == '' || " + value(labels, "app.kubernetes.io/managed-by") + " != 'Helm')", "'flux-helm/' + " + value(labels, "helm.toolkit.fluxcd.io/namespace") + " + '/' + " + helm},
			{argo + " != ''", "'argo/' + " + argo},
			{argo + " == '' && " + argoLabel + " != ''", "'argo-label/' + " + argoLabel},
			{release + " != '' && " + value(labels, "app.kubernetes.io/managed-by") + " == 'Helm'", "'helm/' + " + value(annotations, "meta.helm.sh/release-namespace") + " + '/' + " + release},
		}
		identities := []string{}
		for _, entry := range entries {
			identities = append(identities, "("+entry.condition+" ? ["+entry.identity+"] : [])")
		}
		return strings.Join(identities, " + ")
	}
	variables = append(variables, map[string]any{"name": "newOwners", "expression": ownerExpression("nl", "na")}, map[string]any{"name": "oldOwners", "expression": ownerExpression("ol", "oa")})
	policy := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicy", "metadata": metadata(gitOpsOwnershipPolicy), "spec": map[string]any{
		"failurePolicy": "Fail", "matchConstraints": map[string]any{"matchPolicy": "Equivalent", "namespaceSelector": map[string]any{}, "objectSelector": map[string]any{}, "resourceRules": []any{map[string]any{"operations": []any{"CREATE", "UPDATE"}, "apiGroups": []any{"*"}, "apiVersions": []any{"*"}, "resources": []any{"*"}, "scope": "Namespaced"}}},
		"matchConditions": []any{map[string]any{"name": "main-resources", "expression": "!has(request.subResource) || request.subResource == ''"}},
		"variables":       variables, "validations": []any{
			map[string]any{"expression": "size(variables.newOwners) <= 1", "message": "Porto ownership protection: one resource cannot have competing Flux, Argo CD or native Helm owners."},
			map[string]any{"expression": "size(variables.oldOwners) == 0 || variables.oldOwners == variables.newOwners", "message": "Porto ownership protection: suspend/remove the old reconciler and complete an approved ownership handoff before changing native ownership."},
			map[string]any{"expression": "oldObject == null || size(variables.oldOwners) > 0 || size(variables.newOwners) == 0 || ('porto.dev/gitops-handoff' in variables.oa && 'porto.dev/gitops-handoff-target' in variables.oa && variables.oa['porto.dev/gitops-handoff-target'] == variables.newOwners[0])", "message": "Porto ownership protection: adopting an existing direct-managed resource requires a staged handoff to this exact application owner."},
			map[string]any{"expression": "!('helm.toolkit.fluxcd.io/name' in variables.ol) || ('helm.toolkit.fluxcd.io/name' in variables.nl && variables.ol['helm.toolkit.fluxcd.io/name'] == variables.nl['helm.toolkit.fluxcd.io/name'] && ('helm.toolkit.fluxcd.io/namespace' in variables.ol ? variables.ol['helm.toolkit.fluxcd.io/namespace'] : '') == ('helm.toolkit.fluxcd.io/namespace' in variables.nl ? variables.nl['helm.toolkit.fluxcd.io/namespace'] : ''))", "message": "Porto ownership protection: a Helm release cannot change between Flux HelmRelease controllers."},
			map[string]any{"expression": "!('porto.dev/gitops-controller' in variables.ol) || size(variables.oldOwners) > 0 || size(variables.newOwners) == 0", "message": "Porto ownership protection: application sources cannot adopt controller installation, GitOps control or credential resources."},
			map[string]any{"expression": "oldObject == null || size(variables.oldOwners) > 0 || size(variables.newOwners) == 0 || !has(oldObject.metadata.ownerReferences) || !oldObject.metadata.ownerReferences.exists(ref, has(ref.controller) && ref.controller)", "message": "Porto ownership protection: an existing controller-generated child is not a top-level application adoption target."},
		},
	}}}
	binding := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "admissionregistration.k8s.io/v1", "kind": "ValidatingAdmissionPolicyBinding", "metadata": metadata(gitOpsOwnershipPolicy + "-" + scope.Namespace), "spec": map[string]any{
		"policyName": gitOpsOwnershipPolicy, "validationActions": []any{"Deny"}, "matchResources": map[string]any{"matchPolicy": "Equivalent", "objectSelector": map[string]any{}, "namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": scope.Namespace}}},
	}}}
	return []*unstructured.Unstructured{policy, binding}
}

func gitOpsPolicyTarget(scope GitOpsScope, object *unstructured.Unstructured) Target {
	resource := "validatingadmissionpolicies"
	if object.GetKind() == "ValidatingAdmissionPolicyBinding" {
		resource = "validatingadmissionpolicybindings"
	}
	return Target{Context: scope.Context, Group: "admissionregistration.k8s.io", Version: "v1", Resource: resource, Name: object.GetName()}
}

func (s *session) gitOpsProtection(ctx context.Context, scope GitOpsScope) (bool, error) {
	for _, expected := range gitOpsOwnershipObjects(scope) {
		target := gitOpsPolicyTarget(scope, expected)
		if err := s.require(ctx, target, "get", ""); err != nil {
			return false, err
		}
		current, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, apiError(err)
		}
		if current.GetDeletionTimestamp() != nil || current.GetLabels()[gitOpsOwnerLabel] != expected.GetLabels()[gitOpsOwnerLabel] {
			return false, conflict("The ownership guard name belongs to another installation or is terminating. It was not overwritten.")
		}
		desiredSpec, _, _ := unstructured.NestedMap(expected.Object, "spec")
		currentSpec, _, _ := unstructured.NestedMap(current.Object, "spec")
		left := &unstructured.Unstructured{Object: map[string]any{"spec": desiredSpec}}
		right := &unstructured.Unstructured{Object: map[string]any{"spec": currentSpec}}
		if gitOpsHash(left) != gitOpsHash(right) {
			return false, conflict("The namespace ownership policy/binding differs from its reviewed deny contract. Review it through controller installation authority before mutation.")
		}
		if expected.GetKind() == "ValidatingAdmissionPolicy" {
			observed, _, _ := unstructured.NestedInt64(current.Object, "status", "observedGeneration")
			warnings, _, _ := unstructured.NestedSlice(current.Object, "status", "typeChecking", "expressionWarnings")
			if observed < current.GetGeneration() || len(warnings) > 0 {
				return false, nil
			}
		}
	}
	return true, nil
}

func (m *Manager) previewGitOpsProtection(ctx context.Context, request GitOpsRequest) (GitOpsPreview, error) {
	preview, err := m.previewGitOpsControllerObjects(ctx, request, gitOpsOwnershipObjects(request.GitOpsScope))
	if err == nil {
		preview.Warnings = []string{"This operation enables only the reviewed namespace-scoped ownership admission binding. It does not install, upgrade or claim the existing controller.", "Existing conflicting ownership may cause native reconciliation to be denied until its owners are repaired deliberately. Evaluation errors fail closed; there is no blanket owner-change exception."}
		preview.Consequences = []string{"Enable a native deny admission guard in application namespace " + request.Namespace + ". It prevents competing Flux/Argo CD/native Helm owner changes and protects Porto control resources, without changing unrelated namespace bindings or deleting workloads."}
		m.mu.Lock()
		pending := m.gitOpsPreviews[preview.Token]
		pending.Preview = preview
		m.gitOpsPreviews[preview.Token] = pending
		m.mu.Unlock()
	}
	return preview, err
}

func (m *Manager) waitGitOpsProtection(parent context.Context, scope GitOpsScope, progress func(string) error) error {
	ctx, cancel := context.WithTimeout(parent, time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		client, err := m.client(scope.Context)
		if err != nil {
			return operations.Partial(publicError(err))
		}
		protected, err := client.gitOpsProtection(ctx, scope)
		if err != nil {
			return operations.Partial(publicError(err))
		}
		if protected {
			enforced, err := client.gitOpsEnforcementProbe(ctx, scope)
			if err != nil {
				return operations.Partial(publicError(err))
			}
			if enforced {
				return progress("reviewed namespace ownership guard accepted a synthetic dry run and denied competing owners")
			}
		}
		select {
		case <-ctx.Done():
			return operations.Partial("Ownership guard was submitted but native type-checking/enforcement could not be verified. No application was reconciled.")
		case <-ticker.C:
		}
	}

}

func (s *session) gitOpsEnforcementProbe(ctx context.Context, scope GitOpsScope) (bool, error) {
	target := Target{Context: scope.Context, Namespace: scope.Namespace, Version: "v1", Resource: "configmaps"}
	if err := s.require(ctx, target, "create", ""); err != nil {
		return false, err
	}
	id, err := randomID()
	if err != nil {
		return false, err
	}
	probe := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "porto-gitops-probe-" + id[:12], "namespace": scope.Namespace},
	}}
	options := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict}
	if _, err := s.objects(target).Create(ctx, probe, options); err != nil {
		return false, apiError(err)
	}
	probe.SetLabels(map[string]string{"kustomize.toolkit.fluxcd.io/name": "synthetic-first", "kustomize.toolkit.fluxcd.io/namespace": scope.Namespace, "argocd.argoproj.io/instance": "synthetic-competing"})
	if _, err := s.objects(target).Create(ctx, probe, options); err != nil {
		if (apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) && strings.Contains(err.Error(), "one resource cannot have competing Flux, Argo CD or native Helm owners") {
			return true, nil
		}
		return false, apiError(err)
	}
	return false, nil
}
