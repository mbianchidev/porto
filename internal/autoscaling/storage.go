package autoscaling

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	v1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

func storageProtected(ctx context.Context, c client.Client, nodeName string) (bool, string, error) {
	var volumes corev1.PersistentVolumeList
	if err := c.List(ctx, &volumes); err != nil {
		return true, "", err
	}
	for _, volume := range volumes.Items {
		if volume.Spec.Local == nil && volume.Spec.HostPath == nil {
			continue
		}
		// No affinity is not evidence of safety. Conservatively retain every
		// autoscaled disk for an unscoped local/hostPath persistent volume.
		if volume.Spec.NodeAffinity == nil || volume.Spec.NodeAffinity.Required == nil {
			return true, "unscoped local persistent volume " + volume.Name, nil
		}
		for _, term := range volume.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, expression := range append(term.MatchExpressions, term.MatchFields...) {
				for _, value := range expression.Values {
					if value == nodeName {
						return true, "local persistent volume " + volume.Name, nil
					}
				}
				if expression.Operator != corev1.NodeSelectorOpIn {
					return true, "local persistent volume with non-exact affinity " + volume.Name, nil
				}
			}
		}
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return true, "", err
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.EmptyDir != nil || volume.HostPath != nil {
				return true, fmt.Sprintf("pod %s/%s uses emptyDir or hostPath", pod.Namespace, pod.Name), nil
			}
		}
	}
	return false, "", nil
}

// protectStorage only ever touches verified workers. WhenEmpty consolidation,
// no expiry/repair/force termination, and a second disk check in Delete provide
// conservative scale-down without silently destroying local persistent data.
func (p *localProvider) protectStorage(ctx context.Context) error {
	p.mu.Lock()
	records := make([]claimRecord, 0, len(p.records))
	for _, record := range p.records {
		records = append(records, record)
	}
	p.mu.Unlock()
	for _, record := range records {
		err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			var node corev1.Node
			if err := p.client.Get(ctx, types.NamespacedName{Name: record.NodeName}, &node); err != nil {
				return client.IgnoreNotFound(err)
			}
			if err := verifyNode(&node, record, false); err != nil {
				return err
			}
			if node.Spec.ProviderID != record.ProviderID {
				return nil
			}
			protected, _, err := storageProtected(ctx, p.client, node.Name)
			if err != nil {
				return err
			}
			stored := node.DeepCopy()
			value := "false"
			if protected {
				value = "true"
			}
			if node.Annotations[v1.DoNotDisruptAnnotationKey] == value {
				return nil
			}
			if node.Annotations == nil {
				node.Annotations = map[string]string{}
			}
			node.Annotations[v1.DoNotDisruptAnnotationKey] = value
			return p.client.Patch(ctx, &node, client.MergeFromWithOptions(stored, client.MergeFromWithOptimisticLock{}))
		})
		if apierrors.IsConflict(err) {
			slog.Warn("Storage protection metadata is changing; retrying on the next pass", "cluster", p.cluster, "node", record.NodeName, "error", err)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *localProvider) storageLoop(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := p.protectStorage(ctx); err != nil && ctx.Err() == nil {
			return fmt.Errorf("local storage protection failed; node controller stopped: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
