package autoscaling

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awslabs/operatorpkg/object"
	"github.com/awslabs/operatorpkg/status"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/state/prediction"
)

func TestScaledObjectApplyAuthenticationAndOwnership(t *testing.T) {
	ctx := context.Background()
	m, _ := testManager(t, "kind")
	c := installationClient(t, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "jobs"}},
		&unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "keda.sh/v1alpha1", "kind": "TriggerAuthentication",
			"metadata": map[string]any{"name": "credentials", "namespace": "jobs"},
		}})
	m.connect = func(string) (clusterConnection, error) { return clusterConnection{client: c}, nil }
	if err := writeJSON(filepath.Join(m.directory("dev"), "settings.json"), Config{KEDA: true}); err != nil {
		t.Fatal(err)
	}
	spec := ScaledObject{Namespace: "jobs", Name: "queue", TargetName: "consumer", MaxReplicas: 5,
		Triggers: []Trigger{{Type: "redis", Metadata: map[string]string{"address": "redis.jobs:6379", "listName": "work", "listLength": "5"},
			AuthenticationRef: &AuthenticationReference{Name: "credentials"}}}}
	for range 2 {
		if err := m.ApplyScaledObject(ctx, "dev", spec); err != nil {
			t.Fatal(err)
		}
	}
	scaled := &unstructured.Unstructured{}
	scaled.SetAPIVersion("keda.sh/v1alpha1")
	scaled.SetKind("ScaledObject")
	if err := c.Get(ctx, types.NamespacedName{Namespace: "jobs", Name: "queue"}, scaled); err != nil {
		t.Fatal(err)
	}
	minimum, _, _ := unstructured.NestedInt64(scaled.Object, "spec", "minReplicaCount")
	maximum, _, _ := unstructured.NestedInt64(scaled.Object, "spec", "maxReplicaCount")
	if minimum != 0 || maximum != 5 || scaled.GetLabels()[ownerLabel] != "porto" {
		t.Fatalf("incorrect event-driven scaler: %#v", scaled.Object)
	}
	if err := m.DeleteScaledObject(ctx, "dev", "jobs", "queue"); err != nil {
		t.Fatal(err)
	}
	var target appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Name: "consumer", Namespace: "jobs"}, &target); err != nil {
		t.Fatal("removing a scaler must preserve its workload")
	}
	foreign := scaled.DeepCopy()
	foreign.SetResourceVersion("")
	foreign.SetLabels(nil)
	if err := c.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplyScaledObject(ctx, "dev", spec); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign ScaledObject was adopted: %v", err)
	}
	if err := m.DeleteScaledObject(ctx, "dev", "jobs", "queue"); err == nil || !strings.Contains(err.Error(), "foreign") {
		t.Fatalf("foreign ScaledObject was deleted: %v", err)
	}
}

func TestInvalidScaledObjectsAndMissingKEDA(t *testing.T) {
	spec := ScaledObject{Namespace: "default", Name: "queue", TargetName: "consumer", MaxReplicas: 3,
		Triggers: []Trigger{{Type: "prometheus", Metadata: map[string]string{"query": "up"}}}}
	m, _ := testManager(t, "k3s")
	if err := m.ApplyScaledObject(context.Background(), "dev", spec); err == nil {
		t.Fatal("workload scaler accepted without KEDA")
	}
	for _, change := range []func(*ScaledObject){
		func(s *ScaledObject) { s.MinReplicas = -1 },
		func(s *ScaledObject) { s.MinReplicas = 5 },
		func(s *ScaledObject) { s.MaxReplicas = 0 },
		func(s *ScaledObject) { s.Triggers = nil },
		func(s *ScaledObject) { s.Namespace = "../escape" },
		func(s *ScaledObject) {
			s.Triggers = []Trigger{{Type: "x", Metadata: map[string]string{"x": "y"}, AuthenticationRef: &AuthenticationReference{Name: "x", Kind: "Secret"}}}
		},
	} {
		copy := spec
		change(&copy)
		if err := validateScaledObject(copy); err == nil {
			t.Fatalf("invalid scaler accepted: %+v", copy)
		}
	}
}

func TestNodePoolUsesSafeUpstreamDisruptionAndReadyNodeClass(t *testing.T) {
	ctx := context.Background()
	c := installationClient(t)
	if err := configureNodePool(ctx, c, testConfig()); err != nil {
		t.Fatal(err)
	}
	var pool v1.NodePool
	if err := c.Get(ctx, types.NamespacedName{Name: managedName}, &pool); err != nil {
		t.Fatal(err)
	}
	if pool.Spec.Disruption.ConsolidationPolicy != v1.ConsolidationPolicyWhenEmpty ||
		pool.Spec.Template.Spec.ExpireAfter.Duration != nil || pool.Spec.Template.Spec.TerminationGracePeriod != nil ||
		len(pool.Spec.Template.Spec.Requirements) == 0 || pool.Spec.Template.Labels[ownerLabel] != "porto" {
		t.Fatalf("unsafe upstream nodepool: %+v", pool.Spec)
	}
	var nodeClass PortoNodeClass
	if err := c.Get(ctx, types.NamespacedName{Name: managedName}, &nodeClass); err != nil {
		t.Fatal(err)
	}
	if !nodeClass.StatusConditions().Get(status.ConditionReady).IsTrue() {
		t.Fatal("PortoNodeClass not ready")
	}
	if gvk := object.GVK(&PortoNodeClass{}); gvk.Group != NodeClassGroupVersion.Group || gvk.Kind != "PortoNodeClass" {
		t.Fatalf("upstream GVK resolution is not registered: %s", gvk)
	}
}

func TestUpstreamControllerConstructionAcrossClusters(t *testing.T) {
	ctx, cancel := context.WithCancel(controllerContext(context.Background()))
	defer cancel()
	for range 2 {
		provider, _, c, _ := testProvider(t)
		clk := clock.RealClock{}
		controllers := controllers.NewControllers(ctx, nil, clk, c,
			events.NewRecorder(record.NewFakeRecorder(100)), provider, provider, state.NewCluster(clk, c, provider),
			nodeoverlay.NewInstanceTypeStore(), prediction.NewStore())
		if len(controllers) < 20 {
			t.Fatalf("upstream provisioning/disruption/lifecycle controllers missing: %d", len(controllers))
		}
	}
}
