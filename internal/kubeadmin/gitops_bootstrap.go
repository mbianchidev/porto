package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type gitOpsBundle struct{ version, url, sha256 string }

func gitOpsBundleFor(controller string) gitOpsBundle {
	if controller == "argocd" {
		return gitOpsBundle{ArgoCDVersion, "https://raw.githubusercontent.com/argoproj/argo-cd/v" + ArgoCDVersion + "/manifests/install.yaml", "09bfbb9041b4a8503c6e4fa22cf41c403fa62be17526de353648a4551f5beb77"}
	}
	return gitOpsBundle{FluxVersion, "https://github.com/fluxcd/flux2/releases/download/v" + FluxVersion + "/install.yaml", "a8c838ccd9be5d30eba9df87cd229b7c4eb80c0049fba1bc3b9840829f267f28"}
}

func verifyGitOpsBundle(data []byte, expected string) error {
	sum := sha256.Sum256(data)
	if len(data) == 0 || len(data) > 16*1024*1024 || hex.EncodeToString(sum[:]) != expected {
		return &Error{422, "The controller bundle does not match Porto's reviewed release SHA-256 pin. No mutable or unverified installer was applied."}
	}
	return nil
}

func (m *Manager) gitOpsBundle(ctx context.Context, controller string) ([]byte, error) {
	bundle := gitOpsBundleFor(controller)
	cache := filepath.Join(m.root, "gitops-bundles", controller+"-"+bundle.version+".yaml")
	if data, err := os.ReadFile(cache); err == nil {
		if err := verifyGitOpsBundle(data, bundle.sha256); err != nil {
			return nil, err
		}
		return data, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, &Error{503, "The pinned controller bundle cache is unreadable. Check Porto state storage."}
	}
	policy, err := networking.Default()
	if err != nil {
		return nil, &Error{503, "Porto networking configuration is unavailable for controller bootstrap."}
	}
	transport, err := policy.Transport()
	if err != nil {
		return nil, &Error{503, "Verified HTTPS/proxy configuration is unavailable for controller bootstrap."}
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Minute, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 10 || request.URL.Scheme != "https" || request.URL.User != nil {
			return errors.New("controller bundle redirect is not authorized")
		}
		return nil
	}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, bundle.url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, &Error{503, "The pinned controller bundle could not be downloaded over verified HTTPS. Check source availability, Porto proxy settings and trust; no insecure fallback was used."}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &Error{502, "The pinned controller release download was rejected. Check upstream availability and networking; no alternate version was selected."}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16*1024*1024+1))
	if err != nil {
		return nil, &Error{502, "The pinned controller bundle download was interrupted."}
	}
	if err := verifyGitOpsBundle(data, bundle.sha256); err != nil {
		return nil, err
	}
	if err := config.WriteFileAtomic(cache, data, 0o600); err != nil {
		return nil, &Error{503, "The verified controller bundle could not be cached. Check local storage before bootstrap."}
	}
	return data, nil
}

func gitOpsInstallationOwner(scope GitOpsScope) string {
	scope.Namespace = scope.ControllerNamespace
	return gitOpsOwner(scope, "Controller", scope.Controller)
}

func gitOpsBootstrapObjects(scope GitOpsScope, data []byte) ([]*unstructured.Unstructured, error) {
	expectedNamespace := "flux-system"
	if scope.Controller == "argocd" {
		expectedNamespace = "argocd"
	}
	if scope.ControllerNamespace != expectedNamespace {
		return nil, invalid("Pinned bootstrap uses the controller's standard flux-system or argocd namespace. Existing installations can be discovered in another explicitly selected namespace.")
	}
	objects, err := decodeDocuments(string(data), 16*1024*1024, 500)
	if err != nil {
		return nil, err
	}
	if scope.Controller == "argocd" {
		objects = append([]*unstructured.Unstructured{{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": scope.ControllerNamespace}}}}, objects...)
	}
	result := []*unstructured.Unstructured{}
	for _, object := range objects {
		if err := normalizeGitOpsBootstrapDefaults(object); err != nil {
			return nil, err
		}
		if scope.Controller == "flux" {
			extra := false
			for _, prefix := range []string{"image-automation-controller", "image-reflector-controller", "notification-controller", "source-watcher"} {
				extra = extra || strings.HasPrefix(object.GetName(), prefix)
			}

			if object.GetKind() == "CustomResourceDefinition" {
				group := gitOpsString(object, "spec", "group")
				extra = group != "source.toolkit.fluxcd.io" && group != "kustomize.toolkit.fluxcd.io" && group != "helm.toolkit.fluxcd.io"
			}
			if extra {
				continue
			}
			if subjects, found, _ := unstructured.NestedSlice(object.Object, "subjects"); found {
				filtered := []any{}
				for _, raw := range subjects {
					value, ok := raw.(map[string]any)
					if !ok {
						return nil, invalid("The pinned controller RBAC subjects are invalid.")
					}
					name, _ := value["name"].(string)
					if slices.Contains([]string{"image-automation-controller", "image-reflector-controller", "notification-controller", "source-watcher"}, name) {
						continue
					}
					filtered = append(filtered, raw)
				}
				if len(filtered) == 0 {
					continue
				}
				_ = unstructured.SetNestedSlice(object.Object, filtered, "subjects")
			}
			if object.GetKind() == "Deployment" && (object.GetName() == "kustomize-controller" || object.GetName() == "helm-controller") {
				containers, _, _ := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
				for _, raw := range containers {
					container, ok := raw.(map[string]any)
					if !ok {
						return nil, invalid("The pinned controller containers are invalid.")
					}
					args, _ := container["args"].([]any)
					args = append(args, "--no-cross-namespace-refs=true", "--default-service-account=default")
					if object.GetName() == "kustomize-controller" {
						args = append(args, "--no-remote-bases=true")
					}
					container["args"] = args
				}
				_ = unstructured.SetNestedSlice(object.Object, containers, "spec", "template", "spec", "containers")
			}
		}
		switch object.GetKind() {
		case "Namespace", "CustomResourceDefinition", "ClusterRole", "ClusterRoleBinding":
		default:
			if namespace := object.GetNamespace(); namespace != "" && namespace != scope.ControllerNamespace {
				return nil, invalid("The pinned bootstrap bundle contains an unexpected namespace.")
			}
			object.SetNamespace(scope.ControllerNamespace)
		}
		labels := object.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[gitOpsOwnerLabel], labels[gitOpsControllerLabel] = gitOpsInstallationOwner(scope), scope.Controller
		object.SetLabels(labels)
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[gitOpsInstallation] = gitOpsBundleFor(scope.Controller).version
		object.SetAnnotations(annotations)
		result = append(result, object)
	}
	return result, nil
}

func gitOpsControllerAction(action string) bool {
	return gitOpsControllerInstallationAction(action) || action == "protect-ownership"
}

func gitOpsControllerInstallationAction(action string) bool {
	return slices.Contains([]string{"bootstrap", "upgrade-controller", "remove-controller"}, action)
}

func (m *Manager) PreviewGitOpsController(ctx context.Context, request GitOpsRequest) (GitOpsPreview, error) {
	if err := validateGitOpsScope(request.GitOpsScope); err != nil {
		return GitOpsPreview{}, err
	}
	if !gitOpsControllerAction(request.Action) {
		return GitOpsPreview{}, invalid("Controller lifecycle accepts only explicit bootstrap, upgrade-controller or remove-controller actions.")
	}
	request.Kind, request.Name = "Controller", request.Controller
	if request.Action == "protect-ownership" {
		return m.previewGitOpsProtection(ctx, request)
	}
	if request.Action == "remove-controller" {
		return m.previewGitOpsControllerRemoval(ctx, request)
	}
	document, err := m.gitOpsBundle(ctx, request.Controller)
	if err != nil {
		return GitOpsPreview{}, err
	}
	objects, err := gitOpsBootstrapObjects(request.GitOpsScope, document)
	if err != nil {
		return GitOpsPreview{}, err
	}
	objects = append(objects, gitOpsOwnershipObjects(request.GitOpsScope)...)
	return m.previewGitOpsControllerObjects(ctx, request, objects)
}

func (m *Manager) previewGitOpsControllerObjects(ctx context.Context, request GitOpsRequest, objects []*unstructured.Unstructured) (GitOpsPreview, error) {
	preview := GitOpsPreview{GitOpsScope: request.GitOpsScope, Action: request.Action, Kind: "Controller", Name: request.Controller, Differences: []Difference{}, Warnings: []string{
		"Controller installation authority is separate from application delivery. Only the selected controller is installed; no application, source repository or automatic reconciliation policy is bootstrapped.",
		"Bootstrap changes cluster-wide CRDs/RBAC and namespace-scoped controller components before observing readiness. Failure can leave a partial installation; recover through the same pinned preview and persisted installation inventory.",
	}, Consequences: []string{"Acknowledge this pinned controller's namespace, CRDs, cluster-wide RBAC and workload privileges. Existing namespaces are reused without ownership changes; no foreign installation is overwritten."}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	preview.Consequences = append(preview.Consequences, "The ownership admission binding covers only application namespace "+request.Namespace+". It denies competing native owner changes, controller-generated-child adoption and adoption of Porto control/credential resources. Admission errors fail closed.")
	client, err := m.client(request.Context)
	if err != nil {
		return preview, err
	}
	prepared := storedGitOps{Request: request, Identity: client.identity, Schemas: map[string]string{}, Steps: []gitOpsStep{}, Guards: []gitOpsGuard{}}
	if request.Action == "upgrade-controller" {
		record, err := m.gitOpsControllerRecord(request.GitOpsScope)
		if err != nil {
			return preview, err
		}
		if len(record.Objects) == 0 {
			return preview, conflict("Only a Porto-owned controller installation with retained recovery inventory can be upgraded.")
		}
	}
	for _, object := range objects {
		target, _, err := client.targetForObject(request.Context, request.ControllerNamespace, object)
		if err != nil {
			return preview, err
		}
		current, err := client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return preview, apiError(err)
		}
		if err == nil && object.GetKind() == "Namespace" {
			prepared.Guards = append(prepared.Guards, gitOpsGuardFor(target, current))
			preview.Warnings = append(preview.Warnings, "The existing controller namespace is not adopted, changed or included in controller removal.")
			continue
		}
		if err == nil && current.GetLabels()[gitOpsOwnerLabel] != object.GetLabels()[gitOpsOwnerLabel] {
			return preview, conflict("A bootstrap resource belongs to another installation. Discover its controller and use its installation owner; existing CRDs/RBAC are never force-adopted.")
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, object, false); err != nil {
			safe := apiError(err)
			var known *Error
			if errors.As(safe, &known) {
				return preview, &Error{known.Code, fmt.Sprintf("Controller preview for %s %s/%s: %s", object.GetKind(), target.Namespace, target.Name, known.Message)}
			}
			return preview, safe
		}
	}
	preview.Warnings = append(preview.Warnings, "Pinned bundle SHA-256: "+gitOpsBundleFor(request.Controller).sha256)
	return m.storeGitOpsPreview(prepared, preview)
}

type gitOpsControllerRecord struct {
	Scope          GitOpsScope
	Version, State string
	Objects        []Target
}

func (m *Manager) gitOpsControllerRecord(scope GitOpsScope) (gitOpsControllerRecord, error) {
	data, err := m.vault.Get("gitops-installation-" + gitOpsInstallationOwner(scope))
	if err != nil {
		return gitOpsControllerRecord{}, &Error{503, "The controller installation/recovery inventory is unavailable. Unlock or recover Porto's credential-backed state; a foreign installation was not adopted."}
	}
	var record gitOpsControllerRecord
	if json.Unmarshal(data, &record) != nil || record.Scope.Context != scope.Context || record.Scope.Controller != scope.Controller || record.Scope.ControllerNamespace != scope.ControllerNamespace {
		return record, conflict("The controller recovery inventory does not match this exact context/controller namespace.")
	}
	return record, nil
}

func (m *Manager) persistGitOpsController(prepared storedGitOps, changed map[string]*unstructured.Unstructured, complete bool) error {
	scope := prepared.Request.GitOpsScope
	record := gitOpsControllerRecord{Scope: scope, Version: gitOpsBundleFor(scope.Controller).version, State: "partial", Objects: []Target{}}
	if previous, err := m.gitOpsControllerRecord(scope); err == nil {
		record.Objects = previous.Objects
	} else {
		dataErr := err
		_, readErr := os.Stat(filepath.Join(m.root, "sealed", "gitops-installation-"+gitOpsInstallationOwner(scope)+".sealed"))
		if !errors.Is(readErr, os.ErrNotExist) {
			return dataErr
		}
	}
	for _, step := range prepared.Steps {
		index := slices.IndexFunc(record.Objects, func(target Target) bool { return sameGitOpsTarget(target, step.Target) })
		if object := changed[gitOpsTargetKey(step.Target)]; object != nil {
			target := step.Target
			target.UID, target.ResourceVersion = string(object.GetUID()), object.GetResourceVersion()
			if index >= 0 {
				record.Objects[index] = target
			} else {
				record.Objects = append(record.Objects, target)
			}
		}
	}
	if complete {
		record.State = "installed"
		if prepared.Request.Action == "remove-controller" {
			record.State = "removed"
		}
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := m.vault.Set("gitops-installation-"+gitOpsInstallationOwner(scope), data); err != nil {
		return &Error{503, "The controller installation recovery inventory could not be persisted. Inspect partial native resources before retrying."}
	}
	return nil
}

func (s *session) gitOpsControllerHasNoApplications(ctx context.Context, scope GitOpsScope) error {
	for _, kind := range gitOpsAppKinds(scope.Controller) {
		target := gitOpsTarget(scope, kind, "")
		target.Namespace = ""
		items, err := s.gitOpsList(ctx, target)
		if err != nil {
			return err
		}
		if len(items) > 0 {
			return conflict("Controller removal is blocked while any native application exists, including unrelated namespaces or externally managed roots. Remove/orphan each application deliberately first; CRDs and data are never cascaded away.")
		}
	}
	return nil
}

func (m *Manager) previewGitOpsControllerRemoval(ctx context.Context, request GitOpsRequest) (GitOpsPreview, error) {
	preview := GitOpsPreview{GitOpsScope: request.GitOpsScope, Action: request.Action, Kind: "Controller", Name: request.Controller, Differences: []Difference{}, Warnings: []string{"Only recorded installation resource UIDs are removed. Namespaces, CRDs, native source registrations and unrelated resources remain; no application is uninstalled implicitly."}, Consequences: []string{"Controller removal stops reconciliation. Recover credentials, controller installation inventory and desired sources outside this cluster before proceeding. Namespace and CRD resources are intentionally preserved."}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	client, err := m.client(request.Context)
	if err != nil {
		return preview, err
	}
	if err := client.gitOpsControllerHasNoApplications(ctx, request.GitOpsScope); err != nil {
		return preview, err
	}
	record, err := m.gitOpsControllerRecord(request.GitOpsScope)
	if err != nil {
		return preview, err
	}
	prepared := storedGitOps{Request: request, Identity: client.identity, Schemas: map[string]string{}, Steps: []gitOpsStep{}, Guards: []gitOpsGuard{}}
	for index := len(record.Objects) - 1; index >= 0; index-- {
		target := record.Objects[index]
		if target.Resource == "namespaces" || target.Resource == "customresourcedefinitions" || target.Resource == "validatingadmissionpolicies" || target.Resource == "validatingadmissionpolicybindings" {
			continue
		}
		current, err := client.gitOpsRead(ctx, target)
		if err != nil {
			return preview, err
		}
		if string(current.GetUID()) != target.UID || current.GetLabels()[gitOpsOwnerLabel] != gitOpsInstallationOwner(request.GitOpsScope) {
			return preview, conflict("An installation resource was replaced or transferred. It was not retargeted for controller removal.")
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, nil, true); err != nil {
			return preview, err
		}
	}
	if len(prepared.Steps) == 0 {
		return preview, conflict("No recorded controller components remain to remove. Inspect retained CRDs, namespaces and any partial installation.")
	}
	return m.storeGitOpsPreview(prepared, preview)
}

func (m *Manager) waitGitOpsController(parent context.Context, scope GitOpsScope, progress func(string) error) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		state, err := m.GitOpsDiscover(ctx, scope)
		if err != nil {
			return operations.Partial(publicError(err) + " Controller resources were submitted; inspect the retained recovery inventory.")
		}
		if state.Installed && state.Ready && state.Supported && state.Protected {
			return progress("pinned controller components are ready; no application was installed")
		}
		if err := progress("waiting for pinned controller rollout and required served schemas"); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return operations.Partial("Controller resources were applied but readiness could not be verified. Inspect pinned deployment/StatefulSet conditions, CRDs and the retained recovery inventory.")
		case <-ticker.C:
		}
	}
}
