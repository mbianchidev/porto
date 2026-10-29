package kubeadmin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

type helmDocument struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Manifest  string `json:"manifest"`
	Info      struct {
		Status       string `json:"status"`
		LastDeployed string `json:"last_deployed"`
	} `json:"info"`
	Chart struct {
		Metadata struct{ Name, Version, AppVersion string } `json:"metadata"`
	} `json:"chart"`
	Hooks []struct {
		Name, Kind, Manifest string
		LastRun              struct {
			Phase string `json:"phase"`
		} `json:"last_run"`
	} `json:"hooks"`
}

func releaseStore(scope HelmScope) Target {
	return Target{Context: scope.Context, Namespace: scope.Namespace, Version: "v1", Resource: "secrets"}
}

func (m *Manager) helmReadPermission(ctx context.Context, scope HelmScope) error {
	if err := validateHelmScope(scope); err != nil {
		return err
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return err
	}
	return client.require(ctx, releaseStore(scope), "list", "")
}

func (m *Manager) HelmReleases(ctx context.Context, scope HelmScope) ([]HelmRelease, error) {
	if err := m.helmReadPermission(ctx, scope); err != nil {
		return nil, err
	}
	data, err := m.helm(ctx, scope, nil, "list", "--all", "--output", "json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name, Namespace, Revision, Status, Chart, Updated string
		AppVersion                                        string `json:"app_version"`
	}
	if json.Unmarshal(data, &rows) != nil {
		return nil, &Error{502, "Helm returned malformed release inventory, not an empty namespace."}
	}
	result := make([]HelmRelease, 0, len(rows))
	for _, row := range rows {
		if row.Namespace != scope.Namespace {
			return nil, &Error{502, "Helm inventory returned another namespace; it was not mixed into this selection."}
		}
		revision, err := strconv.Atoi(row.Revision)
		if err != nil {
			return nil, &Error{502, "Helm returned an invalid release revision."}
		}
		result = append(result, HelmRelease{Name: row.Name, Namespace: row.Namespace, Revision: revision, Status: row.Status, Chart: row.Chart, AppVersion: row.AppVersion, Updated: row.Updated})
	}
	return result, nil
}

func (m *Manager) helmRelease(ctx context.Context, scope HelmScope, name string) (helmDocument, error) {
	if !helmName.MatchString(name) {
		return helmDocument{}, invalid("Select a valid Helm release name.")
	}
	if err := m.helmReadPermission(ctx, scope); err != nil {
		return helmDocument{}, err
	}
	data, err := m.helm(ctx, scope, nil, "status", name, "--output", "json")
	if err != nil {
		return helmDocument{}, err
	}
	var result helmDocument
	if json.Unmarshal(data, &result) != nil || result.Name != name || result.Namespace != scope.Namespace || result.Version < 1 {
		return result, &Error{502, "Helm release identity/status is invalid; another release or namespace was not substituted."}
	}
	return result, nil
}

func (m *Manager) HelmRelease(ctx context.Context, scope HelmScope, name string) (HelmRelease, error) {
	document, err := m.helmRelease(ctx, scope, name)
	if err != nil {
		return HelmRelease{}, err
	}
	hooks := []HelmHook{}
	for _, hook := range document.Hooks {
		hooks = append(hooks, HelmHook{Name: hook.Name, Kind: hook.Kind, Phase: hook.LastRun.Phase})
	}
	return HelmRelease{Name: document.Name, Namespace: document.Namespace, Revision: document.Version, Status: document.Info.Status, Chart: document.Chart.Metadata.Name + "-" + document.Chart.Metadata.Version, AppVersion: document.Chart.Metadata.AppVersion, Updated: document.Info.LastDeployed, Hooks: hooks}, nil
}

func (m *Manager) HelmHistory(ctx context.Context, scope HelmScope, name string) ([]HelmRelease, error) {
	if !helmName.MatchString(name) {
		return nil, invalid("Select a valid Helm release name.")
	}
	if err := m.helmReadPermission(ctx, scope); err != nil {
		return nil, err
	}
	data, err := m.helm(ctx, scope, nil, "history", name, "--output", "json")
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Revision               int
		Status, Chart, Updated string
		AppVersion             string `json:"app_version"`
	}
	if json.Unmarshal(data, &rows) != nil {
		return nil, &Error{502, "Helm returned malformed release history."}
	}
	result := make([]HelmRelease, 0, len(rows))
	for _, row := range rows {
		result = append(result, HelmRelease{Name: name, Namespace: scope.Namespace, Revision: row.Revision, Status: row.Status, Chart: row.Chart, AppVersion: row.AppVersion, Updated: row.Updated})
	}
	return result, nil
}

func (m *Manager) HelmValues(ctx context.Context, scope HelmScope, name string, reveal bool) (json.RawMessage, error) {
	if !reveal {
		return nil, &Error{403, "Explicit authorized reveal is required for Helm values; they can contain Secrets."}
	}
	if !helmName.MatchString(name) {
		return nil, invalid("Select a valid Helm release name.")
	}
	if err := m.helmReadPermission(ctx, scope); err != nil {
		return nil, err
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return nil, err
	}
	if err := client.require(ctx, releaseStore(scope), "get", ""); err != nil {
		return nil, err
	}
	data, err := m.helm(ctx, scope, nil, "get", "values", name, "--all", "--output", "json")
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, &Error{502, "Helm values could not be decoded; no fallback values were invented."}
	}
	return json.RawMessage(data), nil
}

func (m *Manager) helmAdmission(ctx context.Context, scope HelmScope, manifest string) ([]*unstructured.Unstructured, []string, error) {
	objects, err := decodeDocuments(manifest, 16*1024*1024, 1000)
	if err != nil {
		return nil, nil, err
	}
	client, err := m.client(scope.Context)
	if err != nil {
		return nil, nil, err
	}
	warnings := []string{}
	for _, object := range objects {
		target, resource, err := client.targetForObject(scope.Context, scope.Namespace, object)
		if err != nil {
			var known *Error
			if errors.As(err, &known) && known.Code == 404 {
				warnings = append(warnings, "A rendered API kind is not currently served. CRD-dependent schema/admission cannot be validated until its API exists; native Helm may fail partially.")
				continue
			}
			return nil, nil, err
		}
		if !resource.Namespaced {
			warnings = append(warnings, "The chart includes a cluster-scoped resource; namespace selection does not confine cluster-wide RBAC or API changes.")
		}
		if err := client.require(ctx, target, "get", ""); err != nil {
			return nil, nil, err
		}
		current, err := client.objects(target).Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if err := client.require(ctx, target, "create", ""); err != nil {
				return nil, nil, err
			}
			if _, err := client.objects(target).Create(ctx, cleanEditable(object), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: "helm", FieldValidation: metav1.FieldValidationStrict}); err != nil {
				return nil, nil, apiError(err)
			}
		} else {
			if err != nil {
				return nil, nil, apiError(err)
			}
			if err := client.require(ctx, target, "patch", ""); err != nil {
				return nil, nil, err
			}
			desired := cleanEditable(object)
			desired.SetResourceVersion(current.GetResourceVersion())
			data, err := json.Marshal(desired.Object)
			if err != nil {
				return nil, nil, err
			}
			if _, err := client.objects(target).Patch(ctx, target.Name, types.MergePatchType, data, metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}, FieldManager: "helm", FieldValidation: metav1.FieldValidationStrict}); err != nil {
				return nil, nil, apiError(err)
			}
		}
	}
	return objects, warnings, nil
}
