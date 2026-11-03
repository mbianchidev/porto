package kubeadmin

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

var gitOpsRevisionSuffix = regexp.MustCompile(`(?:^|@sha1:|@sha256:|sha256:)([a-fA-F0-9]{40}|[a-fA-F0-9]{64})$`)

func gitOpsRevision(value string) string {
	if match := gitOpsRevisionSuffix.FindStringSubmatch(value); len(match) > 1 {
		return strings.ToLower(match[1])
	}
	if len(value) <= 128 && chartVersion.MatchString(value) {
		return value
	}
	return ""
}

func gitOpsTime(value any) string {
	text, _ := value.(string)
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	return ""
}

func gitOpsString(object *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(object.Object, fields...)
	return value
}

func gitOpsBool(object *unstructured.Unstructured, fields ...string) bool {
	value, _, _ := unstructured.NestedBool(object.Object, fields...)
	return value
}

func gitOpsReady(object *unstructured.Unstructured) bool {
	generation, _, _ := unstructured.NestedInt64(object.Object, "status", "observedGeneration")
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok || condition["type"] != "Ready" || condition["status"] != "True" {
			continue
		}
		if observed, ok := condition["observedGeneration"].(int64); ok {
			if observed > generation {
				generation = observed
			}
		}
		return generation >= object.GetGeneration()
	}
	return false
}

func gitOpsFailure(reason string) string {
	switch reason {
	case "AuthenticationFailed", "GitOperationFailed", "GitRepositoryNotReady", "SourceNotReady", "FetchFailed", "ArtifactFailed", "ArtifactOutdated", "StorageOperationFailed", "NoSourceArtifact", "SourceUnavailable":
		return "source"
	case "BuildFailed", "ValidationFailed", "DryRunFailed", "KustomizeBuildFailed", "ComparisonError", "InvalidSpecError":
		return "render/admission"
	case "HealthCheckFailed", "TestFailed", "ProgressDeadlineExceeded", "DependencyNotReady":
		return "workload-health"
	default:
		return "reconciliation"
	}
}

func gitOpsConditions(object *unstructured.Unstructured) ([]GitOpsCondition, string) {
	result := []GitOpsCondition{}
	stage := ""
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := condition["type"].(string)
		state, _ := condition["status"].(string)
		reason, _ := condition["reason"].(string)
		if !slices.Contains([]string{"Ready", "Reconciling", "Stalled", "Released", "TestSuccess", "Healthy", "Degraded", "ComparisonError", "SyncError", "InvalidSpecError", "RepeatedResourceWarning", "SharedResourceWarning"}, kind) {
			continue
		}
		if !slices.Contains([]string{"True", "False", "Unknown"}, state) {
			state = "Unknown"
		}
		// Native messages can contain failed command output, repository tokens or rendered Secrets.
		safeReason := "ControllerReportedCondition"
		if slices.Contains([]string{"ReconciliationSucceeded", "ReconciliationFailed", "AuthenticationFailed", "BuildFailed", "ValidationFailed", "DryRunFailed", "HealthCheckFailed", "DependencyNotReady", "InstallFailed", "UpgradeFailed", "UninstallFailed", "ArtifactFailed", "GitOperationFailed", "Progressing", "ProgressingWithRetry", "Succeeded", "Failed"}, reason) {
			safeReason = reason
		}
		result = append(result, GitOpsCondition{Type: kind, Status: state, Reason: safeReason, TransitionAt: gitOpsTime(condition["lastTransitionTime"])})
		if kind == "Ready" && state == "False" || kind == "Stalled" && state == "True" || strings.HasSuffix(kind, "Error") {
			stage = gitOpsFailure(reason)
			if strings.HasSuffix(kind, "Error") {
				stage = gitOpsFailure(kind)
				if kind == "ComparisonError" {
					message, _ := condition["message"].(string)
					stage = gitOpsArgoFailure(message, stage)
				}
			}
		}
	}
	return result, stage
}

func (s *session) gitOpsList(ctx context.Context, target Target) ([]unstructured.Unstructured, error) {
	if err := s.require(ctx, target, "list", ""); err != nil {
		return nil, err
	}
	options := metav1.ListOptions{Limit: 200}
	result := []unstructured.Unstructured{}
	for page := 0; page < 20; page++ {
		list, err := s.objects(target).List(ctx, options)
		if err != nil {
			return nil, apiError(err)
		}
		result = append(result, list.Items...)
		if list.GetContinue() == "" {
			return result, nil
		}
		options.Continue = list.GetContinue()
	}
	return nil, &Error{503, "GitOps inventory exceeded its bounded pagination limit. Narrow the selected namespace; no partial inventory was treated as complete."}
}

func gitOpsAppKinds(controller string) []string {
	if controller == "argocd" {
		return []string{"Application"}
	}
	return []string{"Kustomization", "HelmRelease"}
}

func (m *Manager) GitOpsApplications(ctx context.Context, scope GitOpsScope) ([]GitOpsApplication, error) {
	if err := validateGitOpsScope(scope); err != nil {
		return nil, err
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return nil, err
	}
	result := []GitOpsApplication{}
	for _, kind := range gitOpsAppKinds(scope.Controller) {
		if _, err := client.gitOpsSchema(ctx, scope, kind); err != nil {
			return nil, err
		}
		items, err := client.gitOpsList(ctx, gitOpsTarget(scope, kind, ""))
		if err != nil {
			return nil, err
		}
		for index := range items {
			object := &items[index]
			if scope.Controller == "argocd" && (gitOpsString(object, "spec", "destination", "namespace") != scope.Namespace || gitOpsString(object, "spec", "destination", "server") != "https://kubernetes.default.svc") {
				continue
			}
			state, err := client.gitOpsState(ctx, scope, object)
			if err != nil {
				return nil, err
			}
			result = append(result, state)
		}
	}
	return result, nil
}

func (m *Manager) GitOpsApplication(ctx context.Context, scope GitOpsScope, kind, name string) (GitOpsApplication, error) {
	if err := validateGitOpsScope(scope); err != nil {
		return GitOpsApplication{}, err
	}
	if !slices.Contains(gitOpsAppKinds(scope.Controller), kind) || len(validation.IsDNS1123Subdomain(name)) > 0 {
		return GitOpsApplication{}, invalid("Select an explicit native application kind and name for this controller.")
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return GitOpsApplication{}, err
	}
	root, err := client.gitOpsRead(ctx, gitOpsTarget(scope, kind, name))
	if err != nil {
		return GitOpsApplication{}, err
	}
	return client.gitOpsState(ctx, scope, root)
}

func (s *session) gitOpsState(ctx context.Context, scope GitOpsScope, root *unstructured.Unstructured) (GitOpsApplication, error) {
	target := gitOpsTarget(scope, root.GetKind(), root.GetName())
	target.UID, target.ResourceVersion = string(root.GetUID()), root.GetResourceVersion()
	state := GitOpsApplication{GitOpsScope: scope, Target: target, Name: root.GetName(), Kind: root.GetKind(), Source: root.GetLabels()[gitOpsSourceLabel],
		Phase: "reconciling", Health: "unknown", Drift: "unknown", DependenciesReady: true,
		Managed:  root.GetLabels()[gitOpsOwnerLabel] == gitOpsOwner(scope, root.GetKind(), root.GetName()),
		Adoption: root.GetAnnotations()["porto.dev/gitops-adoption"], Conditions: []GitOpsCondition{}, Inventory: gitOpsInventory(root)}
	state.Conditions, state.FailureStage = gitOpsConditions(root)
	state.Configuration = GitOpsConfiguration{Path: gitOpsString(root, "spec", "path"), ServiceAccount: gitOpsString(root, "spec", "serviceAccountName"), Dependencies: []string{}}
	suspended := gitOpsBool(root, "spec", "suspend")
	if scope.Controller == "argocd" {
		if gitOpsString(root, "spec", "destination", "namespace") != scope.Namespace || gitOpsString(root, "spec", "destination", "server") != "https://kubernetes.default.svc" {
			return state, &Error{403, "The Argo CD application destination is outside this explicitly selected local context/namespace."}
		}
		suspended = root.GetAnnotations()["argocd.argoproj.io/skip-reconcile"] == "true"
		state.DesiredRevision = gitOpsRevision(gitOpsString(root, "spec", "source", "targetRevision"))
		state.Configuration.Path, state.Configuration.Chart, state.Configuration.Project = gitOpsString(root, "spec", "source", "path"), gitOpsString(root, "spec", "source", "chart"), gitOpsString(root, "spec", "project")
		state.Configuration.Renderer = "directory"
		if _, found, _ := unstructured.NestedMap(root.Object, "spec", "source", "kustomize"); found {
			state.Configuration.Renderer = "kustomize"
		}
		state.AppliedRevision = gitOpsRevision(gitOpsString(root, "status", "sync", "revision"))
		state.SourceRevision = state.AppliedRevision
		state.Prune = gitOpsBool(root, "spec", "syncPolicy", "automated", "prune")
		state.SelfHeal = gitOpsBool(root, "spec", "syncPolicy", "automated", "selfHeal")
		automated, found, _ := unstructured.NestedMap(root.Object, "spec", "syncPolicy", "automated")
		state.Automatic = !suspended && found && automated["enabled"] != false
		state.Drift = map[string]string{"Synced": "in-sync", "OutOfSync": "out-of-sync"}[gitOpsString(root, "status", "sync", "status")]
		if state.Drift == "" {
			state.Drift = "unknown"
		}
		health := gitOpsString(root, "status", "health", "status")
		switch health {
		case "Healthy":
			compared := gitOpsRevision(gitOpsString(root, "status", "sync", "comparedTo", "source", "targetRevision"))
			if state.DesiredRevision != "" && compared == state.DesiredRevision && state.AppliedRevision == state.DesiredRevision && state.Drift == "in-sync" {
				state.Health, state.Phase = "healthy", "ready"
			}
		case "Degraded", "Missing":
			state.Health, state.FailureStage = "degraded", "workload-health"
		case "Progressing", "Suspended":
			state.Health = "progressing"
		}
		phase := gitOpsString(root, "status", "operationState", "phase")
		if phase == "Failed" || phase == "Error" {
			state.FailureStage = gitOpsArgoFailure(gitOpsString(root, "status", "operationState", "message"), "reconciliation")
		}
		history, _, _ := unstructured.NestedSlice(root.Object, "status", "history")
		for _, raw := range history {
			if item, ok := raw.(map[string]any); ok {
				if timestamp := gitOpsTime(item["deployedAt"]); timestamp > state.LastSuccess {
					state.LastSuccess = timestamp
				}
			}
		}
	} else {
		sourceReady := false
		state.Automatic = !suspended
		state.Prune = gitOpsBool(root, "spec", "prune")
		state.SelfHeal = root.GetKind() == "Kustomization" || gitOpsString(root, "spec", "driftDetection", "mode") == "enabled"
		sourceKind, sourceName := gitOpsString(root, "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "sourceRef", "name")
		sourceNamespace := gitOpsString(root, "spec", "sourceRef", "namespace")
		if root.GetKind() == "HelmRelease" {
			state.Prune = true
			state.Configuration.Chart = gitOpsString(root, "spec", "chart", "spec", "chart")
			sourceKind, sourceName = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "chart", "spec", "sourceRef", "name")
			sourceNamespace = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "namespace")
			state.DesiredRevision = gitOpsRevision(gitOpsString(root, "spec", "chart", "spec", "version"))
			if gitOpsReady(root) {
				state.AppliedRevision = gitOpsRevision(gitOpsString(root, "status", "lastAttemptedRevision"))
			}
		} else {
			state.AppliedRevision = gitOpsRevision(gitOpsString(root, "status", "lastAppliedRevision"))
		}
		if sourceNamespace != "" && sourceNamespace != scope.Namespace || sourceKind != "GitRepository" && sourceKind != "HelmRepository" {
			state.FailureStage, state.Message = "source", "This source uses an unsupported kind or cross-namespace reference. It was not followed implicitly."
		} else {
			source, err := s.gitOpsRead(ctx, gitOpsTarget(scope, sourceKind, sourceName))
			if err != nil {
				var known *Error
				if !errors.As(err, &known) || known.Code != 404 {
					return state, err
				}
				state.FailureStage, state.Message = "source", "The exact native source is missing. Restore source access explicitly; no public/global fallback was used."
			} else {
				sourceReady = gitOpsReady(source)
				state.SourceRevision = gitOpsRevision(gitOpsString(source, "status", "artifact", "revision"))
				if sourceKind == "GitRepository" {
					state.DesiredRevision = gitOpsRevision(gitOpsString(source, "spec", "ref", "commit"))
				}
				if _, stage := gitOpsConditions(source); stage != "" {
					state.FailureStage = "source"
				}
				if !gitOpsReady(source) && !suspended && state.FailureStage == "" {
					state.Message = "Waiting for the selected source artifact; source readiness is separate from application readiness."
				}
			}
		}
		dependencies, _, _ := unstructured.NestedSlice(root.Object, "spec", "dependsOn")
		for _, raw := range dependencies {
			dependency, ok := raw.(map[string]any)
			if !ok {
				return state, &Error{422, "A native GitOps dependency is malformed."}
			}
			name, _ := dependency["name"].(string)
			state.Configuration.Dependencies = append(state.Configuration.Dependencies, name)
			namespace, _ := dependency["namespace"].(string)
			if namespace != "" && namespace != scope.Namespace {
				return state, &Error{403, "Cross-namespace dependency inspection is not implicit."}
			}
			value, err := s.gitOpsRead(ctx, gitOpsTarget(scope, root.GetKind(), name))
			if err != nil {
				return state, err
			}
			state.DependenciesReady = state.DependenciesReady && gitOpsReady(value)
		}
		if sourceReady && gitOpsReady(root) && state.DependenciesReady && state.DesiredRevision != "" && state.AppliedRevision == state.DesiredRevision && state.FailureStage == "" {
			state.Phase, state.Drift = "ready", "in-sync"
			if root.GetKind() == "HelmRelease" && !gitOpsBool(root, "spec", "install", "disableWait") && !gitOpsBool(root, "spec", "upgrade", "disableWait") || root.GetKind() == "Kustomization" && gitOpsBool(root, "spec", "wait") {
				state.Health = "healthy"
			}
		} else if state.AppliedRevision != "" && state.AppliedRevision != state.DesiredRevision {
			state.Drift = "out-of-sync"
		}
		history, _, _ := unstructured.NestedSlice(root.Object, "status", "history")
		for _, raw := range history {
			if item, ok := raw.(map[string]any); ok {
				timestamp := ""
				if item["lastReconciledStatus"] == "ReconciliationSucceeded" {
					timestamp = gitOpsTime(item["lastReconciled"])
				}
				if item["status"] == "deployed" {
					timestamp = gitOpsTime(item["lastDeployed"])
				}
				if timestamp > state.LastSuccess {
					state.LastSuccess = timestamp
				}
			}
		}
	}
	if state.FailureStage != "" {
		state.Phase = "failed"
		if state.FailureStage == "workload-health" {
			state.Health = "degraded"
		}
	}
	if suspended {
		state.Phase = "suspended"
		state.Message = "Reconciliation is suspended. Health, revision and inventory values are last observed controller state, not a fresh readiness check."
	}
	if root.GetDeletionTimestamp() != nil {
		state.Phase = "terminating"
		state.Message = "Native deletion is pending controller dependents or finalizers. No finalizer is removed implicitly."
	}
	if state.Message == "" {
		state.Message = "Native reconciliation and workload readiness are separate observations. Raw controller messages and rendered Secret values are withheld."
	}
	return state, nil
}

func gitOpsInventory(root *unstructured.Unstructured) []GitOpsResource {
	result := []GitOpsResource{}
	if root.GetKind() == "Kustomization" {
		entries, _, _ := unstructured.NestedSlice(root.Object, "status", "inventory", "entries")
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := entry["id"].(string)
			version, _ := entry["v"].(string)
			parts := strings.Split(id, "_")
			if len(parts) == 4 {
				result = append(result, GitOpsResource{Namespace: parts[0], Name: parts[1], Group: parts[2], Kind: parts[3], Version: version})
			}
		}
	} else if root.GetKind() == "Application" {
		entries, _, _ := unstructured.NestedSlice(root.Object, "status", "resources")
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			value := func(key string) string { text, _ := entry[key].(string); return text }
			result = append(result, GitOpsResource{Group: value("group"), Version: value("version"), Kind: value("kind"), Namespace: value("namespace"), Name: value("name")})
		}
	}
	return result
}

func gitOpsArgoFailure(message, fallback string) string {
	message = strings.ToLower(message)
	for _, pattern := range []string{"authentication", "authorization failed", "permission denied (publickey", "repository not found", "failed to list refs", "failed to fetch", "could not resolve host", "tls handshake", "x509:"} {
		if strings.Contains(message, pattern) {
			return "source"
		}
	}
	for _, pattern := range []string{"helm template", "kustomize build", "failed to generate manifest", "admission webhook", "is invalid:", "failed to apply", "dry run"} {
		if strings.Contains(message, pattern) {
			return "render/admission"
		}
	}
	return fallback
}
