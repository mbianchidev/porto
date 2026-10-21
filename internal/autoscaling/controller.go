package autoscaling

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/operator/options"
)

func controllerContext(ctx context.Context) context.Context {
	return options.ToContext(ctx, &options.Options{
		DisableControllerWarmup: true, DisableClusterStateObservability: true,
		CPURequests: 1000, KubeClientQPS: 50, KubeClientBurst: 100,
		BatchMaxDuration: 10 * time.Second, BatchIdleDuration: time.Second,
		PreferencePolicy: options.PreferencePolicyRespect, MinValuesPolicy: options.MinValuesPolicyStrict,
		IgnoreDRARequests: true,
		// Optional DRA, node repair, overlay, static capacity, reservation and
		// capacity buffer features are intentionally not enabled.
	})
}

func runController(ctx context.Context, connection clusterConnection, provider *localProvider) error {
	logger := logr.FromSlogHandler(slog.Default().Handler()).WithName("porto-autoscaling").WithValues("cluster", provider.cluster)
	ctx = log.IntoContext(ctx, logger)
	ctx, cancel := context.WithCancel(controllerContext(ctx))
	defer func() {
		cancel()
		// Even lease loss skips controller-runtime's graceful wait. Never let
		// Pause return while a VM mutation or its failure cleanup still runs.
		provider.mu.Lock()
		provider.mu.Unlock()
	}()
	s, err := newScheme()
	if err != nil {
		return err
	}
	shutdownTimeout := -time.Second
	mgr, err := ctrl.NewManager(connection.config, ctrl.Options{
		Scheme:                 s,
		Logger:                 logger,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         true, LeaderElectionID: "porto-karpenter", LeaderElectionNamespace: "kube-system",
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       &shutdownTimeout,
		BaseContext:                   func() context.Context { return controllerContext(ctx) },
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&coordinationv1.Lease{}: {Field: fields.SelectorFromSet(fields.Set{"metadata.namespace": "kube-node-lease"})},
			&v1.NodeClaim{}:         {Label: labels.SelectorFromSet(labels.Set{ownerLabel: "porto"})},
			&v1.NodePool{}:          {Label: labels.SelectorFromSet(labels.Set{ownerLabel: "porto"})},
		}},
	})
	if err != nil {
		return fmt.Errorf("create host Karpenter manager: %w", err)
	}
	if err := setupIndexes(ctx, mgr); err != nil {
		return fmt.Errorf("configure Karpenter cache indexes: %w", err)
	}
	cluster := state.NewCluster(clock.RealClock{}, mgr.GetClient(), provider)
	for _, controller := range controllers.NewControllers(
		ctx, mgr, clock.RealClock{}, mgr.GetClient(),
		events.NewRecorder(mgr.GetEventRecorderFor("porto-karpenter")), // upstream Recorder uses the legacy event interface
		provider, provider, cluster, nodeoverlay.NewInstanceTypeStore(),
	) {
		if err := controller.Register(ctx, mgr); err != nil {
			return fmt.Errorf("register upstream Karpenter controller: %w", err)
		}
	}
	if err := mgr.Add(manager.RunnableFunc(provider.storageLoop)); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

func setupIndexes(ctx context.Context, mgr manager.Manager) error {
	indexes := []struct {
		object client.Object
		field  string
		value  client.IndexerFunc
	}{
		{&corev1.Pod{}, "spec.nodeName", func(o client.Object) []string { return []string{o.(*corev1.Pod).Spec.NodeName} }},
		{&corev1.Node{}, "spec.providerID", func(o client.Object) []string { return []string{o.(*corev1.Node).Spec.ProviderID} }},
		{&storagev1.VolumeAttachment{}, "spec.nodeName", func(o client.Object) []string { return []string{o.(*storagev1.VolumeAttachment).Spec.NodeName} }},
		{&v1.NodeClaim{}, "status.providerID", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Status.ProviderID} }},
		{&v1.NodeClaim{}, "spec.nodeClassRef.group", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Group} }},
		{&v1.NodeClaim{}, "spec.nodeClassRef.kind", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Kind} }},
		{&v1.NodeClaim{}, "spec.nodeClassRef.name", func(o client.Object) []string { return []string{o.(*v1.NodeClaim).Spec.NodeClassRef.Name} }},
		{&v1.NodePool{}, "spec.template.spec.nodeClassRef.group", func(o client.Object) []string {
			return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Group}
		}},
		{&v1.NodePool{}, "spec.template.spec.nodeClassRef.kind", func(o client.Object) []string { return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Kind} }},
		{&v1.NodePool{}, "spec.template.spec.nodeClassRef.name", func(o client.Object) []string { return []string{o.(*v1.NodePool).Spec.Template.Spec.NodeClassRef.Name} }},
	}
	for _, index := range indexes {
		if err := mgr.GetFieldIndexer().IndexField(ctx, index.object, index.field, index.value); err != nil {
			return fmt.Errorf("%s: %w", index.field, err)
		}
	}
	return nil
}
