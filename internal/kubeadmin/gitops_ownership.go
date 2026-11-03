package kubeadmin

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

const gitOpsHandoff = "porto.dev/gitops-handoff"

func gitOpsOwned(object *unstructured.Unstructured) bool {
	if object == nil {
		return false
	}
	labels, annotations := object.GetLabels(), object.GetAnnotations()
	if annotations[gitOpsHandoff] != "" || annotations["argocd.argoproj.io/tracking-id"] != "" || annotations["argocd.argoproj.io/instance"] != "" {
		return true
	}
	for _, key := range []string{"kustomize.toolkit.fluxcd.io/name", "helm.toolkit.fluxcd.io/name", "argocd.argoproj.io/instance", "porto.dev/gitops-controller"} {
		if labels[key] != "" {
			return true
		}
	}
	for _, field := range object.GetManagedFields() {
		switch field.Manager {
		case "kustomize-controller", "helm-controller", "argocd-controller", "argocd-application-controller":
			return true
		}
	}
	return false
}

func rejectGitOpsWriter(object *unstructured.Unstructured) error {
	if gitOpsOwned(object) {
		return conflict("This resource has a GitOps owner or staged ownership handoff. Use its GitOps application and desired-state source; direct administration cannot compete with reconciliation.")
	}
	return nil
}
