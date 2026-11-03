package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

const gitOpsInstallation = "porto.dev/gitops-installation"

type gitOpsAPI struct {
	group, version, resource, kind string
	fields                         []string
}

var gitOpsAPIs = map[string]gitOpsAPI{
	"GitRepository":  {"source.toolkit.fluxcd.io", "v1", "gitrepositories", "GitRepository", []string{"spec.url", "spec.ref.commit", "spec.secretRef.name", "spec.suspend"}},
	"HelmRepository": {"source.toolkit.fluxcd.io", "v1", "helmrepositories", "HelmRepository", []string{"spec.url", "spec.secretRef.name", "spec.suspend"}},
	"Kustomization":  {"kustomize.toolkit.fluxcd.io", "v1", "kustomizations", "Kustomization", []string{"spec.sourceRef.name", "spec.path", "spec.suspend", "spec.serviceAccountName", "spec.prune", "spec.deletionPolicy"}},
	"HelmRelease":    {"helm.toolkit.fluxcd.io", "v2", "helmreleases", "HelmRelease", []string{"spec.chart.spec.chart", "spec.chart.spec.sourceRef.name", "spec.suspend", "spec.serviceAccountName", "spec.driftDetection.mode"}},
	"Application":    {"argoproj.io", "v1alpha1", "applications", "Application", []string{"spec.source.repoURL", "spec.source.targetRevision", "spec.project", "spec.destination.namespace", "spec.syncPolicy.automated.enabled", "operation.sync.revision"}},
	"AppProject":     {"argoproj.io", "v1alpha1", "appprojects", "AppProject", []string{"spec.sourceRepos", "spec.destinations"}},
}

func validateGitOpsScope(scope GitOpsScope) error {
	if strings.TrimSpace(scope.Context) == "" || scope.Controller != "flux" && scope.Controller != "argocd" {
		return invalid("Select an explicit Kubernetes context and either Flux or Argo CD. No controller is installed automatically.")
	}
	if len(validation.IsDNS1123Label(scope.Namespace)) > 0 || len(validation.IsDNS1123Label(scope.ControllerNamespace)) > 0 {
		return invalid("Select explicit valid application and controller namespaces.")
	}
	return nil
}

func gitOpsTarget(scope GitOpsScope, kind, name string) Target {
	api := gitOpsAPIs[kind]
	namespace := scope.Namespace
	if scope.Controller == "argocd" {
		namespace = scope.ControllerNamespace
	}
	return Target{Context: scope.Context, Namespace: namespace, Group: api.group, Version: api.version, Resource: api.resource, Name: name}
}

func (s *session) gitOpsSchema(ctx context.Context, scope GitOpsScope, kind string) (string, error) {
	api, ok := gitOpsAPIs[kind]
	if !ok {
		return "", invalid("This GitOps resource kind is not supported.")
	}
	target := gitOpsTarget(scope, kind, "")
	if _, err := s.resource(target); err != nil {
		return "", err
	}
	crdTarget := Target{Context: scope.Context, Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions", Name: api.resource + "." + api.group}
	if err := s.require(ctx, crdTarget, "get", ""); err != nil {
		return "", err
	}
	crd, err := s.objects(crdTarget).Get(ctx, crdTarget.Name, metav1.GetOptions{})
	if err != nil {
		return "", apiError(err)
	}
	group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
	plural, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "plural")
	declaredKind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
	if group != api.group || plural != api.resource || declaredKind != kind {
		return "", conflict("The installed GitOps CRD identity differs from its served API. Inspect the controller installation.")
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	for _, raw := range versions {
		version, ok := raw.(map[string]any)
		if !ok || version["name"] != api.version || version["served"] != true {
			continue
		}
		definition, found, _ := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
		if !found {
			break
		}
		for _, field := range api.fields {
			node := definition
			for _, segment := range strings.Split(field, ".") {
				properties, _ := node["properties"].(map[string]any)
				node, _ = properties[segment].(map[string]any)
				if node == nil {
					return "", &Error{422, "The installed " + kind + " schema does not support required field " + field + ". Upgrade the controller through its installation owner; no assumed schema was applied."}
				}
			}
		}
		data, err := json.Marshal(definition)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:]), nil
	}
	return "", &Error{422, "The installed GitOps CRD does not serve the required structural schema. Inspect its version and controller compatibility."}
}

type gitOpsComponent struct{ name, resource, image string }

func gitOpsComponents(controller string) []gitOpsComponent {
	if controller == "argocd" {
		return []gitOpsComponent{
			{"argocd-application-controller", "statefulsets", "quay.io/argoproj/argocd:v" + ArgoCDVersion},
			{"argocd-repo-server", "deployments", "quay.io/argoproj/argocd:v" + ArgoCDVersion},
			{"argocd-redis", "deployments", "public.ecr.aws/docker/library/redis:8.2.3-alpine"},
		}
	}
	return []gitOpsComponent{
		{"source-controller", "deployments", "ghcr.io/fluxcd/source-controller:v1.10.1"},
		{"kustomize-controller", "deployments", "ghcr.io/fluxcd/kustomize-controller:v1.10.1"},
		{"helm-controller", "deployments", "ghcr.io/fluxcd/helm-controller:v1.7.1"},
	}
}

func gitOpsKinds(controller string) []string {
	if controller == "argocd" {
		return []string{"Application", "AppProject"}
	}
	return []string{"GitRepository", "HelmRepository", "Kustomization", "HelmRelease"}
}

func (m *Manager) GitOpsDiscover(ctx context.Context, scope GitOpsScope) (GitOpsController, error) {
	state := GitOpsController{GitOpsScope: scope, Supported: true, Ready: true, Managed: true, BootstrapVersion: FluxVersion, Capabilities: []string{}}
	if err := validateGitOpsScope(scope); err != nil {
		return state, err
	}
	if scope.Controller == "argocd" {
		state.BootstrapVersion = ArgoCDVersion
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return state, err
	}
	state.Protected, err = client.gitOpsProtection(ctx, scope)
	if err != nil {
		return state, err
	}
	for _, component := range gitOpsComponents(scope.Controller) {
		target := Target{Context: scope.Context, Namespace: scope.ControllerNamespace, Group: "apps", Version: "v1", Resource: component.resource, Name: component.name}
		if err := client.require(ctx, target, "get", ""); err != nil {
			return state, err
		}
		object, err := client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			state.Ready = false
			continue
		}
		if err != nil {
			return state, apiError(err)
		}
		state.Installed = true
		state.Managed = state.Managed && object.GetAnnotations()[gitOpsInstallation] != ""
		containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		matchingImage := false
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			matchingImage = matchingImage || ok && container["image"] == component.image
		}
		state.Supported = state.Supported && matchingImage
		observed, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
		ready, _, _ := unstructured.NestedInt64(object.Object, "status", "readyReplicas")
		updated, _, _ := unstructured.NestedInt64(object.Object, "status", "updatedReplicas")
		desired, found, _ := unstructured.NestedInt64(object.Object, "spec", "replicas")
		if !found {
			desired = 1
		}
		state.Ready = state.Ready && observed >= object.GetGeneration() && desired > 0 && ready >= desired && updated >= desired
		if component.resource == "deployments" {
			total, _, _ := unstructured.NestedInt64(object.Object, "status", "replicas")
			available, _, _ := unstructured.NestedInt64(object.Object, "status", "availableReplicas")
			state.Ready = state.Ready && total == desired && available >= desired
		} else {
			state.Ready = state.Ready && gitOpsString(object, "status", "currentRevision") == gitOpsString(object, "status", "updateRevision")
		}
	}
	for _, kind := range gitOpsKinds(scope.Controller) {
		_, err := client.gitOpsSchema(ctx, scope, kind)
		if err != nil {
			var known *Error
			if !isGitOpsUnavailableSchema(err, &known) {
				return state, err
			}
			state.Supported = false
			state.Message = known.Message
			continue
		}
		state.Capabilities = append(state.Capabilities, kind)
	}
	kind := "Kustomization"
	if scope.Controller == "argocd" {
		kind = "Application"
	}
	permission, err := client.permission(ctx, gitOpsTarget(scope, kind, ""), "create", "")
	if err != nil {
		return state, err
	}
	state.CanWrite = permission.Allowed
	permission, err = client.permission(ctx, Target{Context: scope.Context, Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, "create", "")
	if err != nil {
		return state, err
	}
	state.CanBootstrap = permission.Allowed
	switch {
	case !state.Installed:
		state.Ready, state.Supported, state.Managed = false, false, false
		state.Message = "No complete controller installation was found in the selected controller namespace. Bootstrap is explicit; existing CRDs alone do not prove a running controller."
	case !state.Supported && state.Message == "":
		state.Message = "Controller images differ from the supported pinned release. Inspect its installation owner and upgrade compatibility before mutation."
	case !state.Ready:
		state.Message = "Controller components are missing or not ready. Inspect their rollout and connectivity before application operations."
	case !state.Protected:
		state.Message = "Controller discovered. Explicitly enable the namespace ownership guard through controller installation authority before application mutation."
	case state.Message == "":
		state.Message = "Supported controller and served schemas discovered. Controller readiness is not application health."
	}
	return state, nil
}

func isGitOpsUnavailableSchema(err error, known **Error) bool {
	if value, ok := err.(*Error); ok && (value.Code == 404 || value.Code == 422) {
		*known = value
		return true
	}
	return false
}

func (m *Manager) requireGitOpsController(ctx context.Context, scope GitOpsScope) error {
	state, err := m.GitOpsDiscover(ctx, scope)
	if err != nil {
		return err
	}
	if !state.Installed || !state.Supported || !state.Ready || !state.Protected {
		return &Error{503, state.Message}
	}
	return nil
}
