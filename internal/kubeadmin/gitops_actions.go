package kubeadmin

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func gitOpsParentOwned(root *unstructured.Unstructured) bool {
	labels, annotations := root.GetLabels(), root.GetAnnotations()
	return labels["kustomize.toolkit.fluxcd.io/name"] != "" || labels["helm.toolkit.fluxcd.io/name"] != "" || labels["argocd.argoproj.io/instance"] != "" || annotations["argocd.argoproj.io/tracking-id"] != "" || len(root.GetOwnerReferences()) > 0
}

func setGitOpsSuspended(root *unstructured.Unstructured, controller string, suspended bool) {
	if controller == "argocd" {
		annotations := root.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if suspended {
			annotations["argocd.argoproj.io/skip-reconcile"] = "true"
		} else {
			delete(annotations, "argocd.argoproj.io/skip-reconcile")
		}
		root.SetAnnotations(annotations)
	} else {
		_ = unstructured.SetNestedField(root.Object, suspended, "spec", "suspend")
	}
}

func gitOpsActive(root *unstructured.Unstructured) bool {
	if operation, found, _ := unstructured.NestedMap(root.Object, "operation"); found && len(operation) > 0 {
		return true
	}
	conditions, _, _ := unstructured.NestedSlice(root.Object, "status", "conditions")
	for _, raw := range conditions {
		if value, ok := raw.(map[string]any); ok && value["type"] == "Reconciling" && value["status"] == "True" {
			return true
		}
	}
	return false
}

func (s *session) gitOpsExclusiveSource(ctx context.Context, scope GitOpsScope, kind, name, appKind, appName string) error {
	for _, rootKind := range gitOpsAppKinds(scope.Controller) {
		items, err := s.gitOpsList(ctx, gitOpsTarget(scope, rootKind, ""))
		if err != nil {
			return err
		}
		for index := range items {
			root := &items[index]
			refKind, refName := gitOpsString(root, "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "sourceRef", "name")
			if rootKind == "HelmRelease" {
				refKind, refName = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "chart", "spec", "sourceRef", "name")
			}
			if refKind == kind && refName == name && (rootKind != appKind || root.GetName() != appName) {
				return conflict("This native GitOps source is referenced by another application. It cannot be changed, suspended or removed as an exclusive application source.")
			}
		}
	}
	return nil
}

func (m *Manager) previewGitOpsLifecycle(ctx context.Context, client *session, prepared storedGitOps, preview GitOpsPreview) (GitOpsPreview, error) {
	request, scope := prepared.Request, prepared.Request.GitOpsScope
	if !slices.Contains(gitOpsAppKinds(scope.Controller), request.Kind) {
		return preview, invalid("Select a native application kind for this controller.")
	}
	target := gitOpsTarget(scope, request.Kind, request.Name)
	root, err := client.gitOpsRead(ctx, target)
	if err != nil {
		return preview, err
	}
	if err := gitOpsUID(request, root); err != nil {
		return preview, err
	}
	if root.GetLabels()[gitOpsOwnerLabel] != gitOpsOwner(scope, request.Kind, request.Name) || gitOpsParentOwned(root) {
		return preview, conflict("This application has an external desired-state owner or has not been registered through Porto. Inspect its parent reconciler and use a controlled ownership handoff; no second writer was created.")
	}
	if gitOpsActive(root) {
		return preview, conflict("The native controller still has an active reconciliation/sync. Wait for its completion or terminate it through its owner before changing configuration.")
	}
	if scope.Controller == "argocd" {
		if gitOpsString(root, "spec", "destination", "namespace") != scope.Namespace || gitOpsString(root, "spec", "destination", "server") != "https://kubernetes.default.svc" {
			return preview, conflict("The application destination changed outside this selected scope.")
		}
		projectTarget := gitOpsTarget(scope, "AppProject", gitOpsString(root, "spec", "project"))
		project, err := client.gitOpsRead(ctx, projectTarget)
		if err != nil {
			return preview, err
		}
		if err := validateGitOpsProject(project, gitOpsString(root, "spec", "source", "repoURL"), scope.Namespace); err != nil {
			return preview, err
		}
		prepared.Guards = append(prepared.Guards, gitOpsGuardFor(projectTarget, project))
	}
	schema, err := client.gitOpsSchema(ctx, scope, request.Kind)
	if err != nil {
		return preview, err
	}
	prepared.Schemas[request.Kind] = schema
	if request.Action == "delete" {
		return m.previewGitOpsDelete(ctx, client, prepared, preview, root)
	}
	if request.Action == "verify-adoption" {
		return m.previewGitOpsAdoptionVerification(ctx, client, prepared, preview, root)
	}
	if root.GetAnnotations()["porto.dev/gitops-adoption"] == "staged" {
		if err := client.guardGitOpsAdoption(ctx, &prepared, root); err != nil {
			return preview, err
		}
	}
	desired := cleanEditable(root)
	var source *unstructured.Unstructured
	var sourceTarget Target
	if scope.Controller == "flux" {
		accountTarget := gitOpsNamedTarget(scope, "v1", "serviceaccounts", gitOpsString(root, "spec", "serviceAccountName"))
		if accountTarget.Name == "" {
			return preview, invalid("An explicit namespace-scoped Flux reconciliation service account is required.")
		}
		account, err := client.gitOpsRead(ctx, accountTarget)
		if err != nil {
			return preview, err
		}
		if err := client.require(ctx, accountTarget, "impersonate", ""); err != nil {
			return preview, err
		}
		prepared.Guards = append(prepared.Guards, gitOpsGuardFor(accountTarget, account))
		kind, name := gitOpsString(root, "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "sourceRef", "name")
		namespace := gitOpsString(root, "spec", "sourceRef", "namespace")
		if request.Kind == "HelmRelease" {
			kind, name = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "chart", "spec", "sourceRef", "name")
			namespace = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "namespace")
		}
		if kind != "GitRepository" && kind != "HelmRepository" || namespace != "" && namespace != scope.Namespace {
			return preview, invalid("Only explicit same-namespace Git/Helm application sources are mutated.")
		}
		sourceTarget = gitOpsTarget(scope, kind, name)
		source, err = client.gitOpsRead(ctx, sourceTarget)
		if err != nil {
			return preview, err
		}
		if source.GetLabels()[gitOpsOwnerLabel] != root.GetLabels()[gitOpsOwnerLabel] {
			return preview, conflict("The native source is not owned exclusively by this application.")
		}
		if err := client.gitOpsExclusiveSource(ctx, scope, kind, name, request.Kind, request.Name); err != nil {
			return preview, err
		}
		schema, err = client.gitOpsSchema(ctx, scope, kind)
		if err != nil {
			return preview, err
		}
		prepared.Schemas[kind] = schema
		source = cleanEditable(source)
	}
	state, err := client.gitOpsState(ctx, scope, root)
	if err != nil {
		return preview, err
	}
	prepared.ExpectedRevision = state.DesiredRevision
	if prepared.ExpectedRevision == "" {
		return preview, invalid("The existing desired source is not an immutable supported revision. Preview a complete source/configuration update first.")
	}
	if slices.Contains([]string{"resume", "reconcile", "recover"}, request.Action) {
		preview.Consequences = append(preview.Consequences, "Controllers render the remote source, not a local workload preview. Review the selected namespace/project and delegated service-account permissions, including unknown namespace, CRD and storage consequences. Native ownership admission rejects competing namespaced owners; it is not permission to grant cluster-wide application privileges.")
	}
	switch request.Action {
	case "update":
		objects, digest, guards, err := m.gitOpsApplicationObjects(ctx, client, request)
		if err != nil {
			return preview, err
		}
		prepared.SourceDigest = digest
		prepared.Guards = append(prepared.Guards, guards...)
		paused := cleanEditable(root)
		setGitOpsSuspended(paused, scope.Controller, true)
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, paused, false); err != nil {
			return preview, err
		}
		for _, object := range objects {
			objectTarget, _, err := client.targetForObject(scope.Context, target.Namespace, object)
			if err != nil {
				return preview, err
			}
			if _, ok := gitOpsAPIs[object.GetKind()]; ok {
				fingerprint, err := client.gitOpsSchema(ctx, scope, object.GetKind())
				if err != nil {
					return preview, err
				}
				prepared.Schemas[object.GetKind()] = fingerprint
			}
			if err := client.gitOpsPrepare(ctx, &prepared, &preview, objectTarget, object, false); err != nil {
				return preview, err
			}
		}
		preview.Warnings = append(preview.Warnings, "Update first suspends the application, then replaces its reviewed source/configuration. It remains suspended until an explicit sync/resume.")
		return m.storeGitOpsPreview(prepared, preview)
	case "recover":
		if scope.Controller == "argocd" {
			chart := gitOpsString(root, "spec", "source", "chart")
			if chart == "" && !gitOpsCommit.MatchString(request.Revision) || chart != "" && !chartVersion.MatchString(request.Revision) {
				return preview, invalid("Recovery requires a compatible immutable Git commit or exact chart version.")
			}
			_ = unstructured.SetNestedField(desired.Object, request.Revision, "spec", "source", "targetRevision")
		} else if request.Kind == "Kustomization" {
			if !gitOpsCommit.MatchString(request.Revision) {
				return preview, invalid("Recovery requires an immutable Git commit.")
			}
			_ = unstructured.SetNestedField(source.Object, request.Revision, "spec", "ref", "commit")
		} else {
			if !chartVersion.MatchString(request.Revision) {
				return preview, invalid("Recovery requires an exact semantic chart version.")
			}
			_ = unstructured.SetNestedField(desired.Object, request.Revision, "spec", "chart", "spec", "version")
		}
		prepared.ExpectedRevision = gitOpsRevision(request.Revision)
		preview.Consequences = append(preview.Consequences, "Recovery reconciles one explicitly selected desired-state revision, then suspends. Verify schema/data compatibility yourself; it cannot restore databases, deleted storage or application data.")
		fallthrough
	case "reconcile":
		prepared.Wait, prepared.OneShot = true, true
		if request.Action == "reconcile" && state.Phase != "suspended" {
			prepared.OneShot = false
		}
		nonce, err := randomID()
		if err != nil {
			return preview, err
		}
		prepared.ReconcileNonce = nonce
		if scope.Controller == "argocd" {
			setGitOpsSuspended(desired, scope.Controller, false)
			desired.Object["operation"] = map[string]any{"sync": map[string]any{"revision": prepared.ExpectedRevision, "prune": request.Prune, "syncOptions": []any{"FailOnSharedResource=true"}}, "info": []any{map[string]any{"name": "porto-request", "value": nonce}}}
			if prepared.OneShot {
				_ = unstructured.SetNestedField(desired.Object, false, "spec", "syncPolicy", "automated", "enabled")
			}
		} else {
			if request.Kind == "Kustomization" {
				_ = unstructured.SetNestedField(desired.Object, request.Prune, "spec", "prune")
			}
			setGitOpsSuspended(desired, scope.Controller, false)
			setGitOpsSuspended(source, scope.Controller, false)
			for _, object := range []*unstructured.Unstructured{desired, source} {
				annotations := object.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations["reconcile.fluxcd.io/requestedAt"] = nonce
				object.SetAnnotations(annotations)
			}
		}
		annotations := desired.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if prepared.OneShot {
			annotations["porto.dev/gitops-once"] = nonce
		}
		desired.SetAnnotations(annotations)
		if state.Prune || request.Prune {
			preview.Consequences = append(preview.Consequences, "This reconciliation has opt-in pruning enabled. Removed resources can include namespace, CRD or storage dependents; inspect the native inventory and reclaim policies.")
		}
		preview.Warnings = append(preview.Warnings, "Manual sync targets the immutable desired revision. A suspended application is resumed for this one attempt and re-suspended after success, failure or cancellation when the API remains reachable. Daemon crashes require explicit inspection and suspension.")
	case "resume":
		if scope.Controller == "flux" && request.Kind == "Kustomization" && !request.SelfHeal {
			return preview, invalid("Running Flux Kustomizations inherently correct drift. Explicitly enable self-heal when resuming continuous reconciliation.")
		}
		if request.Kind == "HelmRelease" && !request.Prune {
			return preview, invalid("Native Helm reconciliation prunes removed chart resources. Explicitly acknowledge native Helm pruning before resuming; it cannot be represented as disabled.")
		}
		setGitOpsSuspended(desired, scope.Controller, false)
		prepared.Wait = true
		if source != nil {
			setGitOpsSuspended(source, scope.Controller, false)
		}
		if scope.Controller == "argocd" {
			_ = unstructured.SetNestedMap(desired.Object, map[string]any{"enabled": request.Automatic, "selfHeal": request.SelfHeal, "prune": request.Prune, "allowEmpty": false}, "spec", "syncPolicy", "automated")
		} else if request.Kind == "Kustomization" {
			_ = unstructured.SetNestedField(desired.Object, request.Prune, "spec", "prune")
		} else {
			mode := "disabled"
			if request.SelfHeal {
				mode = "enabled"
			}
			_ = unstructured.SetNestedField(desired.Object, mode, "spec", "driftDetection", "mode")
		}
		if request.Prune {
			preview.Consequences = append(preview.Consequences, "Continuous pruning is enabled deliberately. Future source updates can remove namespace, CRD or storage dependents without another Porto confirmation.")
		}
		if scope.Controller == "argocd" && !request.Automatic {
			prepared.Wait = false
		}
		preview.Warnings = append(preview.Warnings, "Resume restores native reconciliation. Flux continuously applies desired state while running; Argo CD automatic sync and self-heal use only the separately selected policy.")
	case "suspend":
		setGitOpsSuspended(desired, scope.Controller, true)
		if source != nil {
			setGitOpsSuspended(source, scope.Controller, true)
		}
		preview.Warnings = append(preview.Warnings, "Suspension stops future controller processing; it is not application health or a rollback. A parent controller is never suspended implicitly.")
	}
	if source != nil {
		paused := cleanEditable(root)
		setGitOpsSuspended(paused, scope.Controller, true)
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, paused, false); err != nil {
			return preview, err
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, sourceTarget, source, false); err != nil {
			return preview, err
		}
		if prepared.OneShot {
			prepared.PauseTargets = append(prepared.PauseTargets, sourceTarget)
		}
	}
	if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, desired, false); err != nil {
		return preview, err
	}
	if prepared.OneShot {
		prepared.PauseTargets = append([]Target{target}, prepared.PauseTargets...)
	}
	return m.storeGitOpsPreview(prepared, preview)
}

func (m *Manager) waitGitOps(parent context.Context, client *session, prepared storedGitOps, progress func(string) error) error {
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	request := prepared.Request
	for {
		root, err := client.gitOpsRead(ctx, gitOpsTarget(request.GitOpsScope, request.Kind, request.Name))
		if err != nil {
			return operations.Partial(publicError(err) + " GitOps mutation was accepted; its completion could not be observed.")
		}
		if request.UID != "" && string(root.GetUID()) != request.UID {
			return operations.Partial("Application identity changed during reconciliation; a replacement was not retargeted.")
		}
		state, err := client.gitOpsState(ctx, request.GitOpsScope, root)
		if err != nil {
			return operations.Partial(publicError(err))
		}
		observed := prepared.ReconcileNonce == ""
		if request.Controller == "flux" {
			observed = observed || gitOpsString(root, "status", "lastHandledReconcileAt") == prepared.ReconcileNonce
		} else {
			info, _, _ := unstructured.NestedSlice(root.Object, "status", "operationState", "operation", "info")
			for _, raw := range info {
				if item, ok := raw.(map[string]any); ok && item["name"] == "porto-request" && item["value"] == prepared.ReconcileNonce {
					observed = true
				}
			}
		}
		if observed && state.FailureStage != "" {
			terminal, err := client.gitOpsAttemptFailed(ctx, request.GitOpsScope, root)
			if err != nil {
				return operations.Partial(publicError(err))
			}
			if terminal {
				return operations.Partial("The selected GitOps attempt failed at " + state.FailureStage + ". Inspect source access, served schemas and native workload conditions; raw Secret-bearing diagnostics were withheld.")
			}
		}
		if observed && state.DesiredRevision == prepared.ExpectedRevision && state.AppliedRevision == prepared.ExpectedRevision && state.Health == "healthy" && state.DependenciesReady {
			return progress("intended immutable revision reconciled; native workload health is ready, not an application-level availability guarantee")
		}
		if err := progress(fmt.Sprintf("observing GitOps reconciliation: %s / health %s / drift %s", state.Phase, state.Health, state.Drift)); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return operations.Partial("GitOps mutation was accepted but readiness observation was interrupted or timed out. Inspect current source/reconciliation/workload state; no success or data rollback is implied.")
		case <-ticker.C:
		}
	}
}

func (s *session) pauseGitOps(ctx context.Context, prepared storedGitOps, changed map[string]*unstructured.Unstructured) error {
	for _, target := range prepared.PauseTargets {
		expected := changed[gitOpsTargetKey(target)]
		if expected == nil {
			return conflict("The one-shot resource identity was not recorded.")
		}
		current, err := s.gitOpsRead(ctx, target)
		if err != nil {
			return err
		}
		if current.GetUID() != expected.GetUID() || current.GetGeneration() != expected.GetGeneration() {
			return conflict("The one-shot resource was replaced or its desired configuration changed; automatic suspension was not retargeted.")
		}
		if err := s.require(ctx, target, "update", ""); err != nil {
			return err
		}
		setGitOpsSuspended(current, prepared.Request.Controller, true)
		annotations := current.GetAnnotations()
		delete(annotations, "porto.dev/gitops-once")
		current.SetAnnotations(annotations)
		if _, err := s.objects(target).Update(ctx, current, metav1.UpdateOptions{FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict}); err != nil {
			return apiError(err)
		}
	}
	return nil
}

func gitOpsTerminalCondition(object *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, raw := range conditions {
		value, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if value["type"] == "Stalled" && value["status"] == "True" {
			return true
		}
		reason, _ := value["reason"].(string)
		if value["type"] == "Ready" && value["status"] == "False" && slices.Contains([]string{"AuthenticationFailed", "GitOperationFailed", "FetchFailed", "BuildFailed", "ValidationFailed", "DryRunFailed", "ReconciliationFailed", "HealthCheckFailed", "InstallFailed", "UpgradeFailed", "UninstallFailed", "TestFailed"}, reason) {
			return true
		}
	}
	return false
}

func (s *session) gitOpsAttemptFailed(ctx context.Context, scope GitOpsScope, root *unstructured.Unstructured) (bool, error) {
	if scope.Controller == "argocd" {
		phase := gitOpsString(root, "status", "operationState", "phase")
		return phase == "Failed" || phase == "Error" || phase == "Succeeded" && gitOpsString(root, "status", "health", "status") == "Degraded", nil
	}
	if gitOpsTerminalCondition(root) {
		return true, nil
	}
	kind, name := gitOpsString(root, "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "sourceRef", "name")
	if root.GetKind() == "HelmRelease" {
		kind, name = gitOpsString(root, "spec", "chart", "spec", "sourceRef", "kind"), gitOpsString(root, "spec", "chart", "spec", "sourceRef", "name")
	}
	source, err := s.gitOpsRead(ctx, gitOpsTarget(scope, kind, name))
	if err != nil {
		return false, err
	}
	return gitOpsTerminalCondition(source), nil
}
