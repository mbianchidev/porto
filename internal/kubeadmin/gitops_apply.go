package kubeadmin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

func gitOpsOwnApplyConflict(current *unstructured.Unstructured, err error) bool {
	if current == nil || !apierrors.IsConflict(err) {
		return false
	}
	createdByPorto := false
	for _, entry := range current.GetManagedFields() {
		createdByPorto = createdByPorto || entry.Manager == gitOpsFieldManager && entry.Operation == metav1.ManagedFieldsOperationUpdate && entry.Subresource == ""
	}
	if !createdByPorto {
		return false
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil || len(status.Status().Details.Causes) == 0 {
		return false
	}
	for _, cause := range status.Status().Details.Causes {
		if cause.Type != metav1.CauseTypeFieldManagerConflict || !strings.HasPrefix(cause.Message, `conflict with "`+gitOpsFieldManager+`"`) {
			return false
		}
	}
	return true
}

func (s *session) gitOpsControllerApply(ctx context.Context, target Target, desired, current *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, bool, error) {
	data, err := json.Marshal(desired.Object)
	if err != nil {
		return nil, false, err
	}
	force := false
	options := metav1.PatchOptions{FieldManager: gitOpsFieldManager, Force: &force, FieldValidation: metav1.FieldValidationStrict, DryRun: []string{metav1.DryRunAll}}
	after, err := s.objects(target).Patch(ctx, target.Name, types.ApplyPatchType, data, options)
	if err != nil {
		if !gitOpsOwnApplyConflict(current, err) {
			return nil, false, err
		}
		options.Force = nil
		if !dryRun {
			options.DryRun = nil
		}
		after, err = s.objects(target).Patch(ctx, target.Name, types.MergePatchType, data, options)
		return after, true, err
	}
	if dryRun {
		return after, false, nil
	}
	options.DryRun = nil
	after, err = s.objects(target).Patch(ctx, target.Name, types.ApplyPatchType, data, options)
	return after, false, err
}
