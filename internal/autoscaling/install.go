package autoscaling

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/apis"
)

const kedaManifestURL = "https://github.com/kedacore/keda/releases/download/v" + KEDAVersion + "/keda-" + KEDAVersion + ".yaml"

func downloadManifest(ctx context.Context, url string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("download pinned autoscaling release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download pinned autoscaling release: HTTP %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 32<<20 {
		return nil, errors.New("autoscaling release manifest exceeds 32 MiB")
	}
	return data, nil
}

func decodeManifest(data []byte) ([]*unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []*unstructured.Unstructured
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode autoscaling manifest: %w", err)
		}
		if object.GetKind() == "" {
			continue
		}
		labels := object.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ownerLabel] = "porto"
		object.SetLabels(labels)
		objects = append(objects, object)
	}
	if len(objects) == 0 {
		return nil, errors.New("empty autoscaling installation manifest")
	}
	return objects, nil
}

func checkOwned(ctx context.Context, c client.Client, object *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(object.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(object), existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if existing.GetLabels()[ownerLabel] != "porto" {
		return fmt.Errorf("refusing to take ownership of foreign %s %s/%s; remove or manage that installation separately", object.GetKind(), object.GetNamespace(), object.GetName())
	}
	return nil
}

func installManifest(ctx context.Context, c client.Client, data []byte) error {
	objects, err := decodeManifest(data)
	if err != nil {
		return err
	}
	// Refuse foreign installations before modifying any part of the release.
	for _, object := range objects {
		if err := checkOwned(ctx, c, object); err != nil {
			return err
		}
	}
	for _, object := range objects {
		if err := c.Patch(ctx, object, client.Apply, client.FieldOwner("porto-autoscaling")); err != nil {
			return fmt.Errorf("install %s %s: %w", object.GetKind(), object.GetName(), err)
		}
		if object.GetKind() == "CustomResourceDefinition" {
			if err := waitCRD(ctx, c, object.GetName()); err != nil {
				return err
			}
		}
	}
	return nil
}

func waitCRD(ctx context.Context, c client.Client, name string) error {
	return wait.PollUntilContextTimeout(ctx, time.Second, 2*time.Minute, true, func(ctx context.Context) (bool, error) {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &crd); err != nil {
			return false, err
		}
		for _, condition := range crd.Status.Conditions {
			if condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
			if condition.Type == apiextensionsv1.NamesAccepted && condition.Status == apiextensionsv1.ConditionFalse {
				return false, fmt.Errorf("autoscaling CRD %s rejected: %s", name, condition.Message)
			}
		}
		return false, nil
	})
}

func installKarpenter(ctx context.Context, c client.Client) error {
	for _, data := range [][]byte{apis.NodePoolCRD, apis.NodeClaimCRD, []byte(nodeClassCRD)} {
		if err := installManifest(ctx, c, data); err != nil {
			return err
		}
	}
	return nil
}

func kedaReady(ctx context.Context, c client.Client) error {
	for _, name := range []string{"scaledobjects.keda.sh", "scaledjobs.keda.sh", "triggerauthentications.keda.sh", "clustertriggerauthentications.keda.sh"} {
		if err := waitCRD(ctx, c, name); err != nil {
			return fmt.Errorf("KEDA CRD readiness %s: %w", name, err)
		}
	}
	for _, name := range kedaDeploymentNames {
		if err := wait.PollUntilContextTimeout(ctx, time.Second, 3*time.Minute, true, func(ctx context.Context) (bool, error) {
			var deployment appsv1.Deployment
			if err := c.Get(ctx, types.NamespacedName{Namespace: "keda", Name: name}, &deployment); err != nil {
				return false, err
			}
			if deployment.Labels[ownerLabel] != "porto" {
				return false, fmt.Errorf("KEDA deployment %s is not managed by Porto", name)
			}
			for _, condition := range deployment.Status.Conditions {
				if condition.Type == appsv1.DeploymentProgressing && condition.Reason == "ProgressDeadlineExceeded" {
					return false, fmt.Errorf("KEDA deployment %s failed rollout: %s", name, condition.Message)
				}
			}
			desired := int32(1)
			if deployment.Spec.Replicas != nil {
				desired = *deployment.Spec.Replicas
			}
			return desired > 0 && deployment.Status.ObservedGeneration >= deployment.Generation &&
				deployment.Status.UpdatedReplicas == desired && deployment.Status.AvailableReplicas == desired, nil
		}); err != nil {
			return fmt.Errorf("KEDA %s is not ready: %w", name, err)
		}
	}
	return nil
}

func disableKEDA(ctx context.Context, c client.Client) error {
	// Keep CRDs, objects, RBAC, webhooks and controller installation intact.
	// Turning off workload scaling only scales owned controllers to zero, and
	// is refused while any ScaledObjects/ScaledJobs would lose their controller.
	for _, kind := range []string{"ScaledObjectList", "ScaledJobList"} {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("keda.sh/v1alpha1")
		list.SetKind(kind)
		if err := c.List(ctx, list); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("check existing KEDA resources before disabling: %w", err)
		}
		if len(list.Items) != 0 {
			return errors.New("KEDA cannot be disabled while ScaledObjects or ScaledJobs exist; delete them explicitly or keep KEDA enabled (no workloads were removed)")
		}
	}
	// Retain admission and metrics services so their registered APIService and
	// webhook configurations remain reachable.
	var deployment appsv1.Deployment
	err := c.Get(ctx, types.NamespacedName{Namespace: "keda", Name: "keda-operator"}, &deployment)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if deployment.Labels[ownerLabel] != "porto" {
		return errors.New("refusing to disable a foreign KEDA installation")
	}
	stored := deployment.DeepCopy()
	zero := int32(0)
	deployment.Spec.Replicas = &zero
	return c.Patch(ctx, &deployment, client.MergeFrom(stored))
}
