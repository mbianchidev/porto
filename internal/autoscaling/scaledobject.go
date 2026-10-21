package autoscaling

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type AuthenticationReference struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // TriggerAuthentication or ClusterTriggerAuthentication
}

type Trigger struct {
	Type              string                   `json:"type"`
	Name              string                   `json:"name,omitempty"`
	Metadata          map[string]string        `json:"metadata"`
	AuthenticationRef *AuthenticationReference `json:"authenticationRef,omitempty"`
}

type ScaledObject struct {
	Namespace        string    `json:"namespace"`
	Name             string    `json:"name"`
	TargetName       string    `json:"targetName"`
	TargetKind       string    `json:"targetKind,omitempty"`
	TargetAPIVersion string    `json:"targetAPIVersion,omitempty"`
	MinReplicas      int32     `json:"minReplicas"`
	MaxReplicas      int32     `json:"maxReplicas"`
	PollingInterval  int32     `json:"pollingInterval,omitempty"`
	CooldownPeriod   int32     `json:"cooldownPeriod,omitempty"`
	Triggers         []Trigger `json:"triggers"`
}

func validateScaledObject(s ScaledObject) error {
	if len(validation.IsDNS1123Label(s.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(s.Name)) != 0 ||
		len(validation.IsDNS1123Subdomain(s.TargetName)) != 0 {
		return errors.New("ScaledObject requires valid namespace, name and targetName")
	}
	if s.MinReplicas < 0 || s.MaxReplicas < 1 || s.MaxReplicas > 10000 || s.MinReplicas > s.MaxReplicas {
		return errors.New("ScaledObject requires 0 <= minReplicas <= maxReplicas <= 10000 and maxReplicas >= 1")
	}
	if s.PollingInterval < 0 || s.CooldownPeriod < 0 || len(s.Triggers) == 0 || len(s.Triggers) > 32 {
		return errors.New("ScaledObject requires 1–32 triggers and nonnegative polling/cooldown periods")
	}
	for _, trigger := range s.Triggers {
		if trigger.Type == "" || len(trigger.Metadata) == 0 {
			return errors.New("each KEDA trigger requires a type and metadata")
		}
		if trigger.AuthenticationRef != nil {
			ref := trigger.AuthenticationRef
			if len(validation.IsDNS1123Subdomain(ref.Name)) != 0 ||
				(ref.Kind != "" && ref.Kind != "TriggerAuthentication" && ref.Kind != "ClusterTriggerAuthentication") {
				return errors.New("invalid KEDA authentication reference")
			}
		}
	}
	return nil
}

// ApplyScaledObject creates or updates a Porto-owned event-driven workload
// scaler. Metadata is sent directly to KEDA; secrets belong in referenced
// TriggerAuthentication objects, not in trigger metadata.
func (m *Manager) ApplyScaledObject(ctx context.Context, cluster string, spec ScaledObject) error {
	if err := errors.Join(validateName(cluster), validateScaledObject(spec)); err != nil {
		return err
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	cfg, err := m.settings(cluster)
	if err != nil {
		return err
	}
	if !cfg.KEDA {
		return errors.New("enable KEDA workload scaling before creating a ScaledObject")
	}
	release, err := m.operation(ctx, cluster, "autoscaling-workload")
	if err != nil {
		return err
	}
	defer release()
	if _, err := m.provisioner.Configuration(cluster); err != nil {
		return err
	}
	connection, err := m.connect(cluster)
	if err != nil {
		return err
	}
	if spec.TargetKind == "" {
		spec.TargetKind = "Deployment"
	}
	if spec.TargetAPIVersion == "" {
		spec.TargetAPIVersion = "apps/v1"
	}
	target := &unstructured.Unstructured{}
	target.SetAPIVersion(spec.TargetAPIVersion)
	target.SetKind(spec.TargetKind)
	if err := connection.client.Get(ctx, types.NamespacedName{Namespace: spec.Namespace, Name: spec.TargetName}, target); err != nil {
		return fmt.Errorf("resolve KEDA scale target: %w", err)
	}
	triggers := make([]any, 0, len(spec.Triggers))
	for _, trigger := range spec.Triggers {
		metadata := map[string]any{}
		for key, value := range trigger.Metadata {
			metadata[key] = value
		}
		value := map[string]any{"type": trigger.Type, "metadata": metadata}
		if trigger.Name != "" {
			value["name"] = trigger.Name
		}
		if ref := trigger.AuthenticationRef; ref != nil {
			kind := ref.Kind
			if kind == "" {
				kind = "TriggerAuthentication"
			}
			namespace := spec.Namespace
			if kind == "ClusterTriggerAuthentication" {
				namespace = ""
			}
			auth := &unstructured.Unstructured{}
			auth.SetAPIVersion("keda.sh/v1alpha1")
			auth.SetKind(kind)
			if err := connection.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, auth); err != nil {
				return fmt.Errorf("resolve KEDA authentication reference: %w", err)
			}
			value["authenticationRef"] = map[string]any{"name": ref.Name, "kind": kind}
		}
		triggers = append(triggers, value)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": spec.Name, "namespace": spec.Namespace, "labels": map[string]any{ownerLabel: "porto"}},
		"spec": map[string]any{
			"scaleTargetRef":  map[string]any{"name": spec.TargetName, "kind": spec.TargetKind, "apiVersion": spec.TargetAPIVersion},
			"minReplicaCount": int64(spec.MinReplicas), "maxReplicaCount": int64(spec.MaxReplicas), "triggers": triggers,
		},
	}}
	if spec.PollingInterval != 0 {
		_ = unstructured.SetNestedField(object.Object, int64(spec.PollingInterval), "spec", "pollingInterval")
	}
	if spec.CooldownPeriod != 0 {
		_ = unstructured.SetNestedField(object.Object, int64(spec.CooldownPeriod), "spec", "cooldownPeriod")
	}
	if err := checkOwned(ctx, connection.client, object); err != nil {
		return err
	}
	return connection.client.Patch(ctx, object, client.Apply, client.FieldOwner("porto-autoscaling"))
}

// DeleteScaledObject never deletes an externally managed scaler or its target.
func (m *Manager) DeleteScaledObject(ctx context.Context, cluster, namespace, name string) error {
	if err := validateName(cluster); err != nil {
		return err
	}
	if len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return errors.New("invalid ScaledObject namespace or name")
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	release, err := m.operation(ctx, cluster, "autoscaling-workload-delete")
	if err != nil {
		return err
	}
	defer release()
	if _, err := m.provisioner.Configuration(cluster); err != nil {
		return err
	}
	connection, err := m.connect(cluster)
	if err != nil {
		return err
	}
	object := &unstructured.Unstructured{}
	object.SetAPIVersion("keda.sh/v1alpha1")
	object.SetKind("ScaledObject")
	if err := connection.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, object); err != nil {
		return client.IgnoreNotFound(err)
	}
	if object.GetLabels()[ownerLabel] != "porto" {
		return errors.New("refusing to delete a foreign ScaledObject")
	}
	if err := connection.client.Delete(ctx, object, client.Preconditions{UID: ptrUID(object.GetUID()), ResourceVersion: ptrString(object.GetResourceVersion())}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func ptrUID(value types.UID) *types.UID { return &value }
func ptrString(value string) *string    { return &value }
