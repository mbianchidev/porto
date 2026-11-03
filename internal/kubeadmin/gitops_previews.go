package kubeadmin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	gitOpsOwnerLabel      = "porto.dev/gitops-owner"
	gitOpsControllerLabel = "porto.dev/gitops-controller"
	gitOpsSourceLabel     = "porto.dev/gitops-source"
	gitOpsFieldManager    = "porto-gitops"
)

func gitOpsOwner(scope GitOpsScope, kind, name string) string {
	data, _ := json.Marshal(struct {
		Scope      GitOpsScope
		Kind, Name string
	}{scope, kind, name})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func gitOpsHash(object *unstructured.Unstructured) string {
	data, _ := json.Marshal(cleanEditable(object))
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func gitOpsPublic(object *unstructured.Unstructured) json.RawMessage {
	if object == nil || object.GetKind() == "Secret" {
		return publicObject(object)
	}
	visible := cleanEditable(object)
	annotations := map[string]string{}
	for _, key := range []string{"argocd.argoproj.io/skip-reconcile", "reconcile.fluxcd.io/requestedAt", gitOpsInstallation, gitOpsHandoff, "porto.dev/gitops-adoption", "porto.dev/gitops-namespace", "porto.dev/gitops-controller-namespace", "porto.dev/gitops-once"} {
		if value := object.GetAnnotations()[key]; value != "" {
			annotations[key] = value
		}
	}
	visible.SetAnnotations(annotations)
	if visible.GetKind() == "CustomResourceDefinition" {
		versions, _, _ := unstructured.NestedSlice(visible.Object, "spec", "versions")
		for _, raw := range versions {
			if version, ok := raw.(map[string]any); ok {
				schema, found, _ := unstructured.NestedMap(version, "schema")
				if found {
					data, _ := json.Marshal(schema)
					sum := sha256.Sum256(data)
					version["schema"] = map[string]any{"reviewedSchemaSHA256": hex.EncodeToString(sum[:])}
				}
			}
		}
		_ = unstructured.SetNestedSlice(visible.Object, versions, "spec", "versions")
	}
	for _, field := range [][]string{
		{"spec", "values"}, {"spec", "postBuild", "substitute"},
		{"spec", "source", "helm", "values"}, {"spec", "source", "helm", "valuesObject"},
		{"spec", "source", "helm", "parameters"}, {"spec", "source", "helm", "fileParameters"},
		{"spec", "sources"},
	} {
		if _, found, _ := unstructured.NestedFieldNoCopy(visible.Object, field...); found {
			_ = unstructured.SetNestedField(visible.Object, "[redacted]", field...)
		}
	}
	for _, field := range [][]string{{"spec", "url"}, {"spec", "source", "repoURL"}} {
		if value, found, _ := unstructured.NestedString(visible.Object, field...); found && validateGitOpsURL(value) != nil {
			_ = unstructured.SetNestedField(visible.Object, "[redacted unsafe source URL]", field...)
		}
	}
	data, _ := json.MarshalIndent(visible.Object, "", "  ")
	return data
}

func (s *session) gitOpsRead(ctx context.Context, target Target) (*unstructured.Unstructured, error) {
	if err := s.require(ctx, target, "get", ""); err != nil {
		return nil, err
	}
	object, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return nil, apiError(err)
	}
	if object.GetDeletionTimestamp() != nil {
		return nil, conflict("A selected GitOps resource is already terminating. Inspect its native finalizers; a new operation cannot bypass them.")
	}
	return object, nil
}

func (s *session) gitOpsPrepare(ctx context.Context, prepared *storedGitOps, preview *GitOpsPreview, target Target, desired *unstructured.Unstructured, deleting bool) error {
	if err := s.require(ctx, target, "get", ""); err != nil {
		return err
	}
	current, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return apiError(err)
	}
	if exists && current.GetDeletionTimestamp() != nil {
		return conflict("A selected native GitOps resource is terminating. Resolve its finalizers before retrying; no finalizer is removed implicitly.")
	}
	for index := len(prepared.Steps) - 1; index >= 0; index-- {
		previous := prepared.Steps[index]
		if !sameGitOpsTarget(previous.Target, target) {
			continue
		}
		if previous.Delete {
			return conflict("A GitOps plan cannot reuse an identity after deletion.")
		}
		var staged unstructured.Unstructured
		if json.Unmarshal(previous.Data, &staged) != nil {
			return invalid("The staged GitOps configuration is invalid.")
		}
		staged.SetUID(current.GetUID())
		staged.SetResourceVersion(current.GetResourceVersion())
		current, exists = &staged, true
		break
	}
	step := gitOpsStep{Target: target, Delete: deleting, Apply: gitOpsControllerAction(prepared.Request.Action) && !deleting}
	verb := "create"
	if exists {
		step.Target.UID, step.Target.ResourceVersion = string(current.GetUID()), current.GetResourceVersion()
		step.BeforeHash = gitOpsHash(current)
		verb = "update"
		if step.Apply {
			verb = "patch"
		}
	}
	if deleting {
		if !exists {
			return &Error{404, "The selected native GitOps resource no longer exists. Refresh its immutable identity."}
		}
		verb = "delete"
	} else if exists && current.GetLabels()[gitOpsOwnerLabel] != desired.GetLabels()[gitOpsOwnerLabel] {
		return conflict("A native resource with this name belongs to another application or installation. Porto will not take its ownership or overwrite it.")
	}
	if err := s.require(ctx, target, verb, ""); err != nil {
		return err
	}
	var after *unstructured.Unstructured
	if deleting {
		uid, version := current.GetUID(), current.GetResourceVersion()
		if err := s.objects(target).Delete(ctx, target.Name, metav1.DeleteOptions{DryRun: []string{metav1.DryRunAll}, Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil {
			return apiError(err)
		}
	} else {
		deferredAdmission := false
		if step.Apply && !exists && target.Namespace != "" {
			for _, previous := range prepared.Steps {
				deferredAdmission = deferredAdmission || previous.Target.Resource == "namespaces" && previous.Target.Group == "" && previous.Target.Name == target.Namespace && previous.BeforeHash == ""
			}
		}
		if deferredAdmission {
			after, err = desired.DeepCopy(), nil
			warning := "The controller namespace does not exist yet. Namespaced admission is deferred until the reviewed namespace creation; activation may fail partially on namespace policies, quotas or RBAC."
			if !slices.Contains(preview.Warnings, warning) {
				preview.Warnings = append(preview.Warnings, warning)
			}
		} else if exists {
			desired.SetResourceVersion(current.GetResourceVersion())
			if step.Apply {
				data, encodeErr := json.Marshal(desired.Object)
				if encodeErr != nil {
					return encodeErr
				}
				force := false
				after, err = s.objects(target).Patch(ctx, target.Name, types.ApplyPatchType, data, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: gitOpsFieldManager, Force: &force, FieldValidation: metav1.FieldValidationStrict})
			} else {
				after, err = s.objects(target).Update(ctx, desired, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict})
			}
		} else {
			after, err = s.objects(target).Create(ctx, desired, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict})
		}
		if err != nil {
			return apiError(err)
		}
		stored := after
		if step.Apply {
			stored = desired
		}
		step.Data, err = json.Marshal(cleanEditable(stored))
		if err != nil {
			return err
		}
	}
	preview.Differences = append(preview.Differences, Difference{Target: step.Target, Kind: func() string {
		if desired != nil {
			return desired.GetKind()
		}
		return current.GetKind()
	}(), Before: gitOpsPublic(current), After: gitOpsPublic(after), Managers: []string{gitOpsFieldManager}})
	prepared.Steps = append(prepared.Steps, step)
	return nil
}

func (m *Manager) PreviewGitOps(ctx context.Context, request GitOpsRequest) (GitOpsPreview, error) {
	preview := GitOpsPreview{GitOpsScope: request.GitOpsScope, Action: request.Action, Name: request.Name, Kind: request.Kind, Differences: []Difference{}, Warnings: []string{
		"GitOps configuration is not rendered workload approval. Controllers fetch and render the pinned source using their native RBAC/project boundaries. Hooks and remote bases are executable workload code.",
		"Preview and activation are not a transaction. Interrupted or failed operations can leave partial native resources; inspect this exact application and operation history before retrying.",
		"Revision recovery never restores database schemas, deleted volumes or application data. No force ownership, unrelated application cleanup or automatic data rollback is used.",
	}, Consequences: []string{}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	if err := validateGitOpsScope(request.GitOpsScope); err != nil {
		return preview, err
	}
	if len(validation.IsDNS1123Label(request.Name)) > 0 || len(request.Name) > 40 {
		return preview, invalid("Use an explicit application name of at most 40 lowercase DNS-label characters.")
	}
	if !slices.Contains([]string{"install", "adopt", "verify-adoption", "update", "reconcile", "suspend", "resume", "recover", "delete"}, request.Action) {
		return preview, invalid("Select a supported GitOps lifecycle action.")
	}
	if (request.Action == "install" || request.Action == "adopt" || request.Action == "update") && (request.Automatic || request.SelfHeal || request.Prune) {
		return preview, invalid("Initial registration is suspended. Preview an explicit reconciliation policy before enabling it.")
	}
	if err := m.requireGitOpsController(ctx, request.GitOpsScope); err != nil {
		return preview, err
	}
	client, err := m.client(request.Context)
	if err != nil {
		return preview, err
	}
	prepared := storedGitOps{Request: request, Identity: client.identity, Schemas: map[string]string{}, Steps: []gitOpsStep{}, Guards: []gitOpsGuard{}}
	if request.Action != "install" && request.Action != "adopt" {
		return m.previewGitOpsLifecycle(ctx, client, prepared, preview)
	}
	objects, digest, guards, err := m.gitOpsApplicationObjects(ctx, client, request)
	if err != nil {
		return preview, err
	}
	prepared.SourceDigest, prepared.Guards = digest, guards
	if request.Action == "adopt" {
		if err := m.stageGitOpsAdoption(ctx, client, &prepared, &preview, objects[len(objects)-1]); err != nil {
			return preview, err
		}
	} else if request.Kind == "HelmRelease" {
		stores, err := client.gitOpsHelmStores(ctx, HelmScope{Context: request.Context, Namespace: request.Namespace}, request.Name)
		if err != nil {
			return preview, err
		}
		if len(stores) > 0 {
			return preview, conflict("A native Helm release already exists with this name. Stage an explicit same-release ownership handoff instead of creating another release controller.")
		}
	}
	rootTarget := gitOpsTarget(request.GitOpsScope, request.Kind, request.Name)
	if _, err := client.objects(rootTarget).Get(ctx, request.Name, metav1.GetOptions{}); err == nil {
		return preview, conflict("This application already exists. Refresh its identity and preview an explicit update or ownership handoff.")
	} else if !apierrors.IsNotFound(err) {
		return preview, apiError(err)
	}
	for _, desired := range objects {
		target, _, err := client.targetForObject(request.Context, rootTarget.Namespace, desired)
		if err != nil {
			return preview, err
		}
		if _, ok := gitOpsAPIs[desired.GetKind()]; ok {
			fingerprint, err := client.gitOpsSchema(ctx, request.GitOpsScope, desired.GetKind())
			if err != nil {
				return preview, err
			}
			prepared.Schemas[desired.GetKind()] = fingerprint
		}
		if err := client.gitOpsPrepare(ctx, &prepared, &preview, target, desired, false); err != nil {
			return preview, err
		}
	}
	preview.Warnings = append(preview.Warnings, "The application is registered suspended. Sources and workloads are not reconciled until a separate reviewed sync/resume operation.")
	return m.storeGitOpsPreview(prepared, preview)
}

func (m *Manager) storeGitOpsPreview(prepared storedGitOps, preview GitOpsPreview) (GitOpsPreview, error) {
	token, err := randomID()
	if err != nil {
		return preview, err
	}
	preview.Token = token
	account := "gitops-preview-" + token
	data, err := json.Marshal(prepared)
	if err != nil {
		return preview, err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return preview, err
	}
	if err := writer.Close(); err != nil {
		return preview, err
	}
	data = compressed.Bytes()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, pending := range m.gitOpsPreviews {
		expires, err := time.Parse(time.RFC3339Nano, pending.Preview.ExpiresAt)
		if err != nil || time.Now().After(expires) {
			if err := m.vault.Delete(pending.Account); err != nil {
				return preview, &Error{503, "Expired encrypted GitOps previews could not be removed. Check local state storage."}
			}
			delete(m.gitOpsPreviews, id)
		}
	}
	if len(m.gitOpsPreviews) >= 16 {
		return preview, conflict("Too many pending GitOps previews. Apply or let existing previews expire before retrying.")
	}
	if err := m.vault.Set(account, data); err != nil {
		return preview, &Error{503, "Encrypted GitOps preview storage is unavailable. Unlock the system credential store; no plaintext configuration was retained."}
	}
	m.gitOpsPreviews[token] = pendingGitOps{Preview: preview, Account: account}
	return preview, nil
}

func (s *session) gitOpsCheck(ctx context.Context, target Target, expected string) (*unstructured.Unstructured, error) {
	if err := s.require(ctx, target, "get", ""); err != nil {
		return nil, err
	}
	current, err := s.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
	if expected == "" && apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, apiError(err)
	}
	if expected == "" || string(current.GetUID()) != target.UID || gitOpsHash(current) != expected || current.GetDeletionTimestamp() != nil {
		return nil, conflict("A selected GitOps configuration, ownership boundary or resource identity changed after preview. Refresh and preview again; no replacement was retargeted.")
	}
	return current, nil
}

func (m *Manager) ApplyGitOps(ctx context.Context, scope GitOpsScope, token string, confirm, acknowledgeData bool) (operations.Operation, error) {
	return m.applyGitOps(ctx, scope, token, confirm, acknowledgeData, false)
}

func (m *Manager) ApplyGitOpsController(ctx context.Context, scope GitOpsScope, token string, confirm, acknowledgeData bool) (operations.Operation, error) {
	return m.applyGitOps(ctx, scope, token, confirm, acknowledgeData, true)
}

func (m *Manager) applyGitOps(ctx context.Context, scope GitOpsScope, token string, confirm, acknowledgeData, controller bool) (operations.Operation, error) {
	if !confirm {
		return operations.Operation{}, invalid("Confirm the exact scoped GitOps preview before activation.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.gitOpsPreviews[token]
	if !ok || pending.Preview.GitOpsScope != scope {
		return operations.Operation{}, conflict("A matching explicit context/controller/namespace GitOps preview is required.")
	}
	if gitOpsControllerAction(pending.Preview.Action) != controller {
		return operations.Operation{}, &Error{403, "Controller installation authority and application delivery authority are separate. Use the matching reviewed lifecycle endpoint."}
	}
	expires, err := time.Parse(time.RFC3339Nano, pending.Preview.ExpiresAt)
	if err != nil || time.Now().After(expires) {
		return operations.Operation{}, conflict("GitOps preview expired. Preview current native state again.")
	}
	if len(pending.Preview.Consequences) > 0 && !acknowledgeData {
		return operations.Operation{}, invalid("Acknowledge the previewed pruning, finalizer, namespace, CRD and storage consequences explicitly.")
	}
	data, err := m.vault.Get(pending.Account)
	if err != nil {
		return operations.Operation{}, &Error{503, "The encrypted GitOps preview is unavailable. Recover the credential store and preview again; no mutation was started."}
	}
	var prepared storedGitOps
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return operations.Operation{}, invalid("The encrypted GitOps preview is invalid.")
	}
	decoded, readErr := io.ReadAll(io.LimitReader(reader, 32*1024*1024+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(decoded) > 32*1024*1024 || json.Unmarshal(decoded, &prepared) != nil || prepared.Request.GitOpsScope != scope {
		return operations.Operation{}, invalid("The encrypted GitOps preview is invalid.")
	}
	op, err := m.operations.Start(ctx, contextKey(scope.Context), "gitops."+prepared.Request.Action, func(ctx context.Context, progress func(string) error) error {
		m.mu.Lock()
		m.mu.Unlock()
		return m.runGitOps(ctx, prepared, progress)
	})
	if err != nil {
		return op, err
	}
	delete(m.gitOpsPreviews, token)
	if err := m.vault.Delete(pending.Account); err != nil {
		return op, errors.Join(err, m.operations.Cancel(op.ID))
	}
	return op, nil
}

func (m *Manager) runGitOps(ctx context.Context, prepared storedGitOps, progress func(string) error) error {
	scope := prepared.Request.GitOpsScope
	client, err := m.client(scope.Context)
	if err != nil {
		return operations.Failure(publicError(err))
	}
	if client.identity != prepared.Identity {
		return operations.Failure("The explicitly selected Kubernetes identity changed after GitOps preview. Preview again; no other context was used.")
	}
	if !gitOpsControllerAction(prepared.Request.Action) {
		if err := m.requireGitOpsController(ctx, scope); err != nil {
			return operations.Failure(publicError(err))
		}
	} else {
		if prepared.Request.Action == "remove-controller" {
			if err := client.gitOpsControllerHasNoApplications(ctx, scope); err != nil {
				return operations.Failure(publicError(err))
			}
		}
	}
	if prepared.SourceDigest != "" {
		_, _, digest, err := m.gitOpsSource(scope, prepared.Request.Source)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if digest != prepared.SourceDigest {
			return operations.Failure("The scoped source registration or credentials changed after preview. Preview again; stale access was not forwarded.")
		}
	}
	for kind, expected := range prepared.Schemas {
		current, err := client.gitOpsSchema(ctx, scope, kind)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if current != expected {
			return operations.Failure("An installed GitOps schema changed after preview. Review controller compatibility and preview again.")
		}
	}
	for _, guard := range prepared.Guards {
		if _, err := client.gitOpsCheck(ctx, guard.Target, guard.Hash); err != nil {
			return operations.Failure(publicError(err))
		}
	}
	if prepared.Request.Action == "verify-adoption" {
		root, err := client.gitOpsRead(ctx, gitOpsTarget(scope, prepared.Request.Kind, prepared.Request.Name))
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if _, err := client.gitOpsAdoptionProof(ctx, scope, root); err != nil {
			return operations.Failure(publicError(err))
		}
	}
	checked := map[string]bool{}
	for _, step := range prepared.Steps {
		if !step.Delete && scope.Controller == "flux" {
			var desired unstructured.Unstructured
			if json.Unmarshal(step.Data, &desired) != nil {
				return operations.Failure("Encrypted native GitOps configuration is invalid.")
			}
			if desired.GetKind() == "Kustomization" || desired.GetKind() == "HelmRelease" {
				account := gitOpsString(&desired, "spec", "serviceAccountName")
				if account == "" {
					return operations.Failure("An explicit namespace-scoped Flux service account must remain configured; no elevated default was used.")
				}
				if err := client.require(ctx, gitOpsNamedTarget(scope, "v1", "serviceaccounts", account), "impersonate", ""); err != nil {
					return operations.Failure(publicError(err))
				}
			}
		}
		key := gitOpsTargetKey(step.Target)
		if !checked[key] {
			if _, err := client.gitOpsCheck(ctx, step.Target, step.BeforeHash); err != nil {
				return operations.Failure(publicError(err))
			}
			checked[key] = true
		}
		verb := "create"
		if step.BeforeHash != "" {
			verb = "update"
			if step.Apply {
				verb = "patch"
			}
		}
		if step.Delete {
			verb = "delete"
		}
		if err := client.require(ctx, step.Target, verb, ""); err != nil {
			return operations.Failure(publicError(err))
		}
	}
	applied := 0
	changed := map[string]*unstructured.Unstructured{}
	if gitOpsControllerInstallationAction(prepared.Request.Action) {
		if err := m.persistGitOpsController(prepared, changed, false); err != nil {
			return operations.Failure(publicError(err))
		}
	}
	for index, step := range prepared.Steps {
		if err := ctx.Err(); err != nil {
			if applied > 0 {
				return operations.Partial("GitOps operation was interrupted after native mutations. Inspect exact application and source identities before retrying; no rollback is implied.")
			}
			return err
		}
		if err := progress(fmt.Sprintf("applying reviewed native GitOps resource %d of %d", index+1, len(prepared.Steps))); err != nil {
			return err
		}
		current, err := client.gitOpsCheck(ctx, step.Target, step.BeforeHash)
		if err != nil {
			if applied > 0 {
				return operations.Partial(publicError(err) + " Earlier native resources were already changed.")
			}
			return operations.Failure(publicError(err))
		}
		if step.Delete {
			uid, version := current.GetUID(), current.GetResourceVersion()
			propagation := metav1.DeletePropagationForeground
			err = client.objects(step.Target).Delete(ctx, step.Target.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}, PropagationPolicy: &propagation})
			if err == nil {
				err = client.observeGitOpsDeletion(ctx, step.Target, uid)
				if err != nil {
					return operations.Partial(publicError(err) + " Deletion may still be pending native finalizers; dependent source cleanup was not continued.")
				}
			}
		} else {
			var desired unstructured.Unstructured
			if json.Unmarshal(step.Data, &desired) != nil {
				return operations.Partial("Encrypted native GitOps configuration could not be decoded. Inspect any earlier resources before retrying.")
			}
			if current == nil {
				changed[gitOpsTargetKey(step.Target)], err = client.objects(step.Target).Create(ctx, &desired, metav1.CreateOptions{FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict})
			} else {
				desired.SetResourceVersion(current.GetResourceVersion())
				if step.Apply {
					data, encodeErr := json.Marshal(desired.Object)
					if encodeErr != nil {
						return mutationError(encodeErr, applied)
					}
					force := false
					changed[gitOpsTargetKey(step.Target)], err = client.objects(step.Target).Patch(ctx, step.Target.Name, types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: gitOpsFieldManager, Force: &force, FieldValidation: metav1.FieldValidationStrict})
				} else {
					changed[gitOpsTargetKey(step.Target)], err = client.objects(step.Target).Update(ctx, &desired, metav1.UpdateOptions{FieldManager: gitOpsFieldManager, FieldValidation: metav1.FieldValidationStrict})
				}
			}
		}
		if err != nil {
			return mutationError(err, applied)
		}
		applied++
		if gitOpsControllerInstallationAction(prepared.Request.Action) {
			if err := m.persistGitOpsController(prepared, changed, false); err != nil {
				return operations.Partial(publicError(err))
			}
		}
	}
	if gitOpsControllerAction(prepared.Request.Action) {
		if prepared.Request.Action == "protect-ownership" {
			return m.waitGitOpsProtection(ctx, scope, progress)
		}
		if prepared.Request.Action != "remove-controller" {
			if err := m.waitGitOpsController(ctx, scope, progress); err != nil {
				return err
			}
		}
		if err := m.persistGitOpsController(prepared, changed, true); err != nil {
			return operations.Partial(publicError(err))
		}
		return progress("reviewed controller lifecycle completed; namespaces, CRDs and unrelated workloads were not removed")
	}
	if prepared.Wait {
		result := m.waitGitOps(ctx, client, prepared, progress)
		if prepared.OneShot {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			if err := client.pauseGitOps(cleanupCtx, prepared, changed); err != nil {
				return operations.Partial("One-shot reconciliation ended, but re-suspension could not be verified. Inspect native state and explicitly suspend this application before retrying. " + operations.PublicMessage(result))
			}
		}
		return result
	}
	if prepared.Request.Action == "delete" {
		return progress("selected GitOps application and exclusively owned source resources removed according to the reviewed orphan/prune policy")
	}
	return progress("native GitOps configuration accepted; inspect current reconciliation policy and operation history")
}

func gitOpsGuardFor(target Target, object *unstructured.Unstructured) gitOpsGuard {
	target.UID, target.ResourceVersion = string(object.GetUID()), object.GetResourceVersion()
	return gitOpsGuard{Target: target, Hash: gitOpsHash(object)}
}

func gitOpsNamedTarget(scope GitOpsScope, version, resource, name string) Target {
	return Target{Context: scope.Context, Namespace: scope.Namespace, Version: version, Resource: resource, Name: name}
}

func gitOpsUID(request GitOpsRequest, current *unstructured.Unstructured) error {
	if request.UID == "" || request.ResourceVersion == "" || request.UID != string(current.GetUID()) || request.ResourceVersion != current.GetResourceVersion() {
		return conflict("Refresh the exact GitOps application UID/resourceVersion before previewing a lifecycle change.")
	}

	return nil
}

func gitOpsTargetKey(target Target) string {
	return target.Context + "/" + target.Group + "/" + target.Version + "/" + target.Resource + "/" + target.Namespace + "/" + target.Name
}

func sameGitOpsTarget(left, right Target) bool {
	return gitOpsTargetKey(left) == gitOpsTargetKey(right)
}
