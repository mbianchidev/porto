package kubeadmin

import (
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestGitOpsOwnedCreateManagerMigrationRejectsForeignWriters(t *testing.T) {
	current := object(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"synthetic","managedFields":[{"manager":"porto-gitops","operation":"Update","apiVersion":"v1","fieldsType":"FieldsV1","fieldsV1":{}}]}}`)
	conflict := func(manager string) error {
		return apierrors.NewApplyConflict([]metav1.StatusCause{{Type: metav1.CauseTypeFieldManagerConflict, Field: ".spec.synthetic", Message: `conflict with "` + manager + `" using v1: .spec.synthetic`}}, "synthetic conflict")
	}
	if !gitOpsOwnApplyConflict(current, conflict("porto-gitops")) {
		t.Fatal("the same installation manager could not update its own created fields")
	}
	if gitOpsOwnApplyConflict(current, conflict("foreign-controller")) {
		t.Fatal("a foreign field manager was displaced")
	}
	current.SetUID(types.UID("synthetic"))
	current.SetManagedFields(nil)
	if gitOpsOwnApplyConflict(current, conflict("porto-gitops")) {
		t.Fatal("manager migration was accepted without matching current provenance")
	}
}
