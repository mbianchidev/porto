package kubeadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
)

var chartVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

func (m *Manager) chartPath(id string) (string, error) {
	if !chartIdentity(id) {
		return "", invalid("A SHA-256-pinned chart archive is required.")
	}
	directory := filepath.Join(m.root, "charts")
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", invalid("The selected chart cache is unavailable. Upload or pull the chart again.")
	}
	defer root.Close()
	file, err := root.Open(id + ".tgz")
	if err != nil {
		return "", invalid("The selected chart archive is unavailable. Upload or pull it again.")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
		return "", invalid("The selected chart archive is unavailable or exceeds 16 MiB. Upload or pull it again.")
	}
	data, err := io.ReadAll(io.LimitReader(file, 16*1024*1024+1))
	if err != nil {
		return "", err
	}
	if len(data) > 16*1024*1024 {
		return "", invalid("The selected chart archive exceeds 16 MiB.")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != id {
		return "", conflict("Chart content changed after it was pinned. No mutable replacement was accepted.")
	}
	return filepath.Join(directory, id+".tgz"), nil
}

func (m *Manager) pullChart(ctx context.Context, request HelmRequest) (string, error) {
	if request.ChartID != "" {
		_, err := m.chartPath(request.ChartID)
		return request.ChartID, err
	}
	if !helmName.MatchString(request.Repository) || !helmName.MatchString(request.Chart) || !chartVersion.MatchString(request.Version) || len(request.Version) > 128 {
		return "", invalid("Select a configured repository, chart and exact semantic chart version, or upload a chart archive.")
	}
	repositories, err := m.HelmRepositories()
	if err != nil {
		return "", err
	}
	found := false
	for _, repository := range repositories {
		if repository.Name == request.Repository {
			found = true
		}
	}
	if !found {
		return "", invalid("The selected Helm repository is not configured in Porto.")
	}
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(m.root, ".chart-pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	m.repoMu.Lock()
	_, err = m.helm(ctx, HelmScope{}, nil, "pull", request.Repository+"/"+request.Chart, "--version", request.Version, "--destination", work)
	m.repoMu.Unlock()
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		return "", err
	}
	var selected string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tgz") {
			if selected != "" {
				return "", invalid("Helm pull returned more than one chart archive.")
			}
			selected = filepath.Join(work, entry.Name())
		}
	}
	if selected == "" {
		return "", &Error{502, "Helm pull did not produce the selected chart archive."}
	}
	file, err := os.Open(selected)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return m.UploadHelmChart(file)
}

func releaseManifests(document helmDocument) string {
	result := document.Manifest
	for _, hook := range document.Hooks {
		if strings.TrimSpace(hook.Manifest) != "" {
			result += "\n---\n" + hook.Manifest
		}
	}
	return result
}

func renderedPublic(manifest string) (json.RawMessage, []string, error) {
	if strings.TrimSpace(manifest) == "" {
		return json.RawMessage("[]"), []string{}, nil
	}
	objects, err := decodeDocuments(manifest, 16*1024*1024, 1000)
	if err != nil {
		return nil, nil, err
	}
	values := []json.RawMessage{}
	storage := []string{}
	for _, object := range objects {
		values = append(values, publicObject(object))
		switch object.GetKind() {
		case "PersistentVolume", "PersistentVolumeClaim", "Namespace", "CustomResourceDefinition":
			storage = append(storage, fmt.Sprintf("%s %s/%s can affect persistent data or dependent resources. Verify native reclaim/keep/finalizer policies before lifecycle changes.", object.GetKind(), object.GetNamespace(), object.GetName()))
		}
	}
	data, err := json.MarshalIndent(values, "", "  ")
	return data, storage, err
}

func (m *Manager) PreviewHelm(ctx context.Context, request HelmRequest) (HelmPreview, error) {
	if err := validateHelmScope(request.HelmScope); err != nil {
		return HelmPreview{}, err
	}
	if !helmName.MatchString(request.Release) || !slices.Contains([]string{"upgrade", "rollback", "uninstall"}, request.Action) {
		return HelmPreview{}, invalid("Select a release and install/upgrade, rollback or uninstall action.")
	}
	if !request.SecretConsent {
		return HelmPreview{}, invalid("Authorize chart/release rendering explicitly; values may include Secrets. Public previews and operation history keep Kubernetes Secret values hidden.")
	}
	client, err := m.client(request.Context)
	if err != nil {
		return HelmPreview{}, err
	}
	for _, verb := range []string{"list", "get", "update"} {
		if err := client.require(ctx, releaseStore(request.HelmScope), verb, ""); err != nil {
			return HelmPreview{}, err
		}
	}
	if request.Action == "upgrade" || request.Action == "rollback" {
		if err := client.require(ctx, releaseStore(request.HelmScope), "create", ""); err != nil {
			return HelmPreview{}, err
		}
	}
	current, err := m.helmRelease(ctx, request.HelmScope, request.Release)
	if err != nil && !missingHelmRelease(err) {
		return HelmPreview{}, err
	}
	revision := current.Version
	if revision == 0 && request.Action != "upgrade" {
		return HelmPreview{}, &Error{404, "The selected release does not exist in this exact namespace."}
	}
	releaseIdentity, err := client.releaseIdentity(ctx, request.HelmScope, request.Release, revision)
	if err != nil {
		return HelmPreview{}, err
	}
	before, beforeStorage, err := renderedPublic(releaseManifests(current))
	if err != nil {
		return HelmPreview{}, err
	}
	after := json.RawMessage("[]")
	warnings := []string{"Helm hooks and templates are trusted workload code. Dry run is not a transaction or application-health guarantee; failed hooks may leave partial resources. No force ownership, automatic data rollback or cleanup is enabled."}
	storage := beforeStorage
	selectedChart := ""
	switch request.Action {
	case "upgrade":
		values := request.Values
		if values == "" {
			values = "{}"
			request.Values = values
		}
		var mapping map[string]any
		if len(values) > 1024*1024 || json.Unmarshal([]byte(values), &mapping) != nil || mapping == nil {
			return HelmPreview{}, invalid("Helm values must be a JSON object of at most 1 MiB. Values are encrypted, never command arguments.")
		}
		selectedChart, err = m.pullChart(ctx, request)
		if err != nil {
			return HelmPreview{}, err
		}
		path, err := m.chartPath(selectedChart)
		if err != nil {
			return HelmPreview{}, err
		}
		output, err := m.helm(ctx, request.HelmScope, []byte(values), "upgrade", request.Release, path, "--install", "--reset-values", "--dry-run=server", "--output", "json", "--timeout", "3m")
		if err != nil {
			return HelmPreview{}, err
		}
		var rendered helmDocument
		if json.Unmarshal(output, &rendered) != nil || rendered.Name != request.Release || rendered.Namespace != request.Namespace {
			return HelmPreview{}, &Error{502, "Helm dry run returned an invalid release identity or manifest document."}
		}
		manifest := releaseManifests(rendered)
		_, admissionWarnings, err := m.helmAdmission(ctx, request.HelmScope, manifest)
		if err != nil {
			return HelmPreview{}, err
		}
		warnings = append(warnings, admissionWarnings...)
		var afterStorage []string
		after, afterStorage, err = renderedPublic(manifest)
		if err != nil {
			return HelmPreview{}, err
		}
		storage = append(storage, afterStorage...)
		warnings = append(warnings, "Values replace prior overrides (--reset-values). Explicitly reveal/load existing values if retaining them. Chart bytes and values are pinned; random templates, admission defaults and hooks can still differ at activation.")
	case "rollback":
		if request.Revision < 1 {
			return HelmPreview{}, invalid("Select an explicit retained Helm revision.")
		}
		history, err := m.HelmHistory(ctx, request.HelmScope, request.Release)
		if err != nil {
			return HelmPreview{}, err
		}
		found := false
		for _, entry := range history {
			if entry.Revision == request.Revision {
				found = true
			}
		}
		if !found {
			return HelmPreview{}, &Error{404, "The requested Helm revision is not retained by this selected release."}
		}
		output, err := m.helm(ctx, request.HelmScope, nil, "get", "manifest", request.Release, "--revision", strconv.Itoa(request.Revision))
		if err != nil {
			return HelmPreview{}, err
		}
		after, storage, err = renderedPublic(string(output))
		if err != nil {
			return HelmPreview{}, err
		}
		warnings = append(warnings, "Rollback restores a retained Helm chart/configuration revision, not database schemas or persistent data. Native hooks and current API compatibility still apply.")
	case "uninstall":
		if _, err := m.helm(ctx, request.HelmScope, nil, "uninstall", request.Release, "--dry-run", "--keep-history"); err != nil {
			return HelmPreview{}, err
		}
		warnings = append(warnings, "Uninstall deletes chart-managed resources according to native Helm policies and retains release history. Storage, namespace or CRD templates may delete dependent data; inspect the rendered inventory.")
	}
	token, err := randomID()
	if err != nil {
		return HelmPreview{}, err
	}
	preview := HelmPreview{Token: token, HelmScope: request.HelmScope, Release: request.Release, Action: request.Action, Revision: revision, ChartID: selectedChart, Before: before, After: after, Warnings: warnings, StorageConsequences: storage, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	prepared := storedHelm{Request: request, Identity: client.identity, ChartID: selectedChart, Revision: revision, ReleaseIdentity: releaseIdentity}
	data, err := json.Marshal(prepared)
	if err != nil {
		return HelmPreview{}, err
	}
	account := "helm-" + token
	if err := m.vault.Set(account, data); err != nil {
		return HelmPreview{}, &Error{503, "Encrypted Helm preview storage is unavailable. Unlock the system credential store; values were not retained in plaintext."}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, current := range m.helmPreviews {
		expiry, _ := time.Parse(time.RFC3339Nano, current.Preview.ExpiresAt)
		if time.Now().After(expiry) {
			if err := m.vault.Delete(current.Account); err != nil {
				return HelmPreview{}, err
			}
			delete(m.helmPreviews, id)
		}
	}
	if len(m.helmPreviews) >= 16 {
		if err := m.vault.Delete(account); err != nil {
			return HelmPreview{}, err
		}
		return HelmPreview{}, conflict("Too many pending Helm previews.")
	}
	m.helmPreviews[token] = pendingHelm{Preview: preview, Account: account}
	return preview, nil
}

func (m *Manager) ApplyHelm(ctx context.Context, scope HelmScope, token string, confirm, acknowledgeStorage bool) (operations.Operation, error) {
	if !confirm {
		return operations.Operation{}, invalid("Confirm the exact Helm lifecycle preview before activation.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.helmPreviews[token]
	if !ok || pending.Preview.HelmScope != scope {
		return operations.Operation{}, conflict("A matching explicit context/namespace Helm preview is required.")
	}
	expires, err := time.Parse(time.RFC3339Nano, pending.Preview.ExpiresAt)
	if err != nil || time.Now().After(expires) {
		return operations.Operation{}, conflict("Helm preview expired. Preview current state again.")
	}
	if len(pending.Preview.StorageConsequences) > 0 && !acknowledgeStorage {
		return operations.Operation{}, invalid("Acknowledge rendered storage/namespace/CRD lifecycle consequences explicitly.")
	}
	data, err := m.vault.Get(pending.Account)
	if err != nil {
		return operations.Operation{}, &Error{503, "The encrypted Helm preview is unavailable. No release operation was started."}
	}
	var prepared storedHelm
	if err := json.Unmarshal(data, &prepared); err != nil {
		return operations.Operation{}, invalid("Stored Helm preview is invalid.")
	}
	op, err := m.operations.Start(ctx, contextKey(scope.Context), "helm."+prepared.Request.Action, func(ctx context.Context, progress func(string) error) error {
		m.mu.Lock()
		m.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		client, err := m.client(scope.Context)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if client.identity != prepared.Identity {
			return operations.Failure("Kubernetes identity changed after Helm preview. Preview again; no global identity was substituted.")
		}
		current, err := m.helmRelease(ctx, scope, prepared.Request.Release)
		if err != nil && !missingHelmRelease(err) {
			return operations.Failure(publicError(err))
		}
		if current.Version != prepared.Revision {
			return operations.Failure("The selected Helm release revision changed after preview. Refresh history and preview again.")
		}
		releaseIdentity, err := client.releaseIdentity(ctx, scope, prepared.Request.Release, current.Version)
		if err != nil {
			return operations.Failure(publicError(err))
		}
		if releaseIdentity != prepared.ReleaseIdentity {
			return operations.Failure("The selected Helm release store changed or was recreated after preview. A reused release name was not retargeted.")
		}
		if err := progress("running the approved Helm action in the exact selected context/namespace"); err != nil {
			return err
		}
		request := prepared.Request
		var args []string
		var values []byte
		switch request.Action {
		case "upgrade":
			path, err := m.chartPath(prepared.ChartID)
			if err != nil {
				return operations.Failure(publicError(err))
			}
			args = []string{"upgrade", request.Release, path, "--install", "--reset-values", "--wait=watcher", "--wait-for-jobs", "--timeout", "3m", "--output", "json"}
			values = []byte(request.Values)
		case "rollback":
			args = []string{"rollback", request.Release, strconv.Itoa(request.Revision), "--wait=watcher", "--wait-for-jobs", "--timeout", "3m"}
		case "uninstall":
			args = []string{"uninstall", request.Release, "--keep-history", "--cascade", "foreground", "--wait=watcher", "--timeout", "3m"}
		}
		if _, err := m.helm(ctx, scope, values, args...); err != nil {
			return operations.Partial(publicError(err) + " Helm may retain partial resources or failed hooks. Inspect this namespace's release status/history; no automatic cleanup or rollback was performed.")
		}
		status, err := m.HelmRelease(ctx, scope, request.Release)
		if err != nil {
			return operations.Partial("Helm command completed but the selected release could not be verified. Inspect native release history and resource conditions.")
		}
		expected := "deployed"
		if request.Action == "uninstall" {
			expected = "uninstalled"
		}
		if status.Status != expected {
			return operations.Partial("The selected Helm release is not in the expected completed state. Inspect release history and hook conditions.")
		}
		return nil
	})
	if err != nil {
		return op, err
	}
	delete(m.helmPreviews, token)
	if err := m.vault.Delete(pending.Account); err != nil {
		return op, errors.Join(err, m.operations.Cancel(op.ID))
	}
	return op, nil
}
