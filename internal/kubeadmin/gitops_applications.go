package kubeadmin

import (
	"context"
	"path"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

var gitOpsCommit = regexp.MustCompile(`^(?:[a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

func validateGitOpsPath(value string) error {
	clean := path.Clean(value)
	if value == "" || len(value) > 1024 || path.IsAbs(value) || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(value, "\\\x00\r\n") {
		return invalid("Select a bounded repository-relative manifest/Kustomize path without parent traversal.")
	}
	return nil
}

func gitOpsObject(scope GitOpsScope, kind, name, owner string, spec map[string]any) *unstructured.Unstructured {
	api := gitOpsAPIs[kind]
	target := gitOpsTarget(scope, kind, name)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.group + "/" + api.version, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": target.Namespace,
			"labels": map[string]any{gitOpsOwnerLabel: owner, gitOpsControllerLabel: scope.Controller}},
		"spec": spec,
	}}
	return object
}

func (m *Manager) gitOpsApplicationObjects(ctx context.Context, client *session, request GitOpsRequest) ([]*unstructured.Unstructured, string, []gitOpsGuard, error) {
	scope := request.GitOpsScope
	if scope.Controller == "flux" && request.Kind != "Kustomization" && request.Kind != "HelmRelease" || scope.Controller == "argocd" && request.Kind != "Application" {
		return nil, "", nil, invalid("Select a native application kind supported by the selected controller.")
	}
	source, credential, digest, err := m.gitOpsSource(scope, request.Source)
	if err != nil {
		return nil, "", nil, err
	}
	if source.Kind == "git" {
		if !gitOpsCommit.MatchString(request.Revision) {
			return nil, "", nil, invalid("GitOps Git sources require an immutable 40- or 64-digit commit, not a mutable branch, tag or HEAD.")
		}
		if err := validateGitOpsPath(request.Path); err != nil {
			return nil, "", nil, err
		}
	} else if !chartVersion.MatchString(request.Revision) || len(request.Revision) > 128 || len(validation.IsDNS1123Subdomain(request.Chart)) > 0 {
		return nil, "", nil, invalid("A Helm GitOps source requires an explicit chart name and exact semantic chart version.")
	}
	if request.Kind == "Kustomization" && source.Kind != "git" || request.Kind == "HelmRelease" && source.Kind != "helm" {
		return nil, "", nil, invalid("Flux Kustomizations use Git sources; this HelmRelease workflow uses a version-pinned Helm repository chart.")
	}
	owner := gitOpsOwner(scope, request.Kind, request.Name)
	objects := []*unstructured.Unstructured{}
	guards := []gitOpsGuard{}
	secretName, sourceName := request.Name+"-access", request.Name+"-source"
	if credential != nil {
		namespace := scope.Namespace
		data := map[string]any{"username": credential.Username, "password": credential.Password}
		labels := map[string]any{gitOpsOwnerLabel: owner, gitOpsControllerLabel: scope.Controller}
		if scope.Controller == "argocd" {
			namespace = scope.ControllerNamespace
			data["url"], data["type"], data["project"] = source.URL, source.Kind, request.Project
			labels["argocd.argoproj.io/secret-type"] = "repository"
		}
		objects = append(objects, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": secretName, "namespace": namespace, "labels": labels}, "type": "Opaque", "stringData": data}})
	}
	if scope.Controller == "flux" {
		if len(validation.IsDNS1123Subdomain(request.ServiceAccount)) > 0 {
			return nil, "", nil, invalid("Select an explicit namespace-scoped Flux reconciliation service account.")
		}
		target := gitOpsNamedTarget(scope, "v1", "serviceaccounts", request.ServiceAccount)
		account, err := client.gitOpsRead(ctx, target)
		if err != nil {
			return nil, "", nil, err
		}
		if err := client.require(ctx, target, "impersonate", ""); err != nil {
			return nil, "", nil, err
		}
		guards = append(guards, gitOpsGuardFor(target, account))
		sourceKind := "GitRepository"
		sourceSpec := map[string]any{"url": source.URL, "interval": "1m", "timeout": "1m", "suspend": true}
		if source.Kind == "git" {
			sourceSpec["ref"] = map[string]any{"commit": strings.ToLower(request.Revision)}
		} else {
			sourceKind = "HelmRepository"
			sourceSpec["passCredentials"] = false
		}
		if credential != nil {
			sourceSpec["secretRef"] = map[string]any{"name": secretName}
		}
		sourceObject := gitOpsObject(scope, sourceKind, sourceName, owner, sourceSpec)
		objects = append(objects, sourceObject)
		spec := map[string]any{"interval": "5m", "timeout": "3m", "suspend": true, "serviceAccountName": request.ServiceAccount, "targetNamespace": scope.Namespace}
		if request.Kind == "Kustomization" {
			spec["sourceRef"] = map[string]any{"kind": sourceKind, "name": sourceName}
			spec["path"], spec["prune"], spec["deletionPolicy"], spec["force"], spec["wait"] = request.Path, false, "Orphan", false, true
		} else {
			spec["releaseName"], spec["storageNamespace"] = request.Name, scope.Namespace
			spec["chart"] = map[string]any{"spec": map[string]any{"chart": request.Chart, "version": request.Revision, "sourceRef": map[string]any{"kind": sourceKind, "name": sourceName}, "interval": "1m"}}
			spec["driftDetection"] = map[string]any{"mode": "disabled"}
			spec["install"] = map[string]any{"createNamespace": false, "disableTakeOwnership": true, "remediation": map[string]any{"retries": int64(0)}}
			spec["upgrade"] = map[string]any{"disableTakeOwnership": true, "remediation": map[string]any{"retries": int64(0)}}
		}
		dependencies, dependencyGuards, err := client.gitOpsDependencyGraph(ctx, request)
		if err != nil {
			return nil, "", nil, err
		}
		guards = append(guards, dependencyGuards...)
		if len(dependencies) > 0 {
			spec["dependsOn"] = dependencies
		}
		root := gitOpsObject(scope, request.Kind, request.Name, owner, spec)
		root.SetAnnotations(map[string]string{"porto.dev/gitops-namespace": scope.Namespace, "porto.dev/gitops-controller-namespace": scope.ControllerNamespace})
		labels := root.GetLabels()
		labels[gitOpsSourceLabel] = source.Name
		root.SetLabels(labels)
		objects = append(objects, root)
	} else {
		if len(validation.IsDNS1123Label(request.Project)) > 0 {
			return nil, "", nil, invalid("Select an explicit Argo CD project that authorizes this exact repository and destination namespace.")
		}
		target := gitOpsTarget(scope, "AppProject", request.Project)
		project, err := client.gitOpsRead(ctx, target)
		if err != nil {
			return nil, "", nil, err
		}
		if err := validateGitOpsProject(project, source.URL, scope.Namespace); err != nil {
			return nil, "", nil, err
		}
		guards = append(guards, gitOpsGuardFor(target, project))
		appSource := map[string]any{"repoURL": source.URL, "targetRevision": request.Revision}
		if source.Kind == "helm" {
			appSource["chart"] = request.Chart
			appSource["helm"] = map[string]any{"releaseName": request.Name, "passCredentials": false}
		} else {
			appSource["path"] = request.Path
			switch request.Renderer {
			case "kustomize":
				appSource["kustomize"] = map[string]any{}
			case "", "directory":
				appSource["directory"] = map[string]any{"recurse": true}
			default:
				return nil, "", nil, invalid("Choose the built-in directory or Kustomize renderer; executable plugins are not enabled by this workflow.")
			}
		}
		root := gitOpsObject(scope, "Application", request.Name, owner, map[string]any{
			"project": request.Project, "source": appSource,
			"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": scope.Namespace},
			"syncPolicy":  map[string]any{"syncOptions": []any{"FailOnSharedResource=true"}},
		})
		root.SetAnnotations(map[string]string{"argocd.argoproj.io/skip-reconcile": "true", "porto.dev/gitops-namespace": scope.Namespace, "porto.dev/gitops-controller-namespace": scope.ControllerNamespace})
		labels := root.GetLabels()
		labels[gitOpsSourceLabel] = source.Name
		root.SetLabels(labels)
		objects = append(objects, root)
	}
	return objects, digest, guards, nil
}

func validateGitOpsProject(project *unstructured.Unstructured, address, namespace string) error {
	repositories, _, _ := unstructured.NestedStringSlice(project.Object, "spec", "sourceRepos")
	sourceAllowed := false
	for _, value := range repositories {
		sourceAllowed = sourceAllowed || value == address
	}
	destinations, _, _ := unstructured.NestedSlice(project.Object, "spec", "destinations")
	destinationAllowed := false
	for _, raw := range destinations {
		value, ok := raw.(map[string]any)
		destinationAllowed = destinationAllowed || ok && value["namespace"] == namespace && value["server"] == "https://kubernetes.default.svc"
	}
	clusterKinds, _, _ := unstructured.NestedSlice(project.Object, "spec", "clusterResourceWhitelist")
	if !sourceAllowed || !destinationAllowed || len(clusterKinds) > 0 {
		return &Error{403, "The selected Argo CD project must explicitly allow this exact source URL and local destination namespace, with no cluster-resource allowlist. Wildcard-only sources/destinations and cluster-scoped workload privileges are not accepted."}
	}
	return nil
}
