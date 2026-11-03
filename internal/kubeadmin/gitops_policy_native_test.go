package kubeadmin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/operations"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func TestNativeGitOpsOwnershipGuard(t *testing.T) {
	filename, name := os.Getenv("PORTO_TEST_GITOPS_KUBECONFIG"), os.Getenv("PORTO_TEST_GITOPS_CONTEXT")
	if filename == "" && name == "" {
		t.Skip("set the explicit disposable GitOps acceptance context to verify native admission")
	}
	if filename == "" || !strings.Contains(name, "porto-gitops-acceptance-") || os.Getenv("PORTO_TEST_GITOPS_CONFIRM") != "disposable" {
		t.Fatal("native ownership acceptance requires an explicitly confirmed disposable porto-gitops-acceptance-* context")
	}
	raw, err := clientcmd.LoadFromFile(filename)
	if err != nil {
		t.Fatal("unable to read the explicit disposable kubeconfig")
	}
	configs := nativeGitOpsConfig{name: name, raw: *raw}
	config, _, err := configs.AdminConfig(name)
	if err != nil {
		t.Fatal(err)
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	id, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	scope := GitOpsScope{Context: name, Namespace: "porto-gitops-owner-" + id[:6], Controller: "flux", ControllerNamespace: "flux-system"}
	if _, err := core.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: scope.Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PORTO_HOME", t.TempDir())
	root := t.TempDir()
	ops := operations.New(filepath.Join(root, "operations"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := ops.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	manager := New(root, configs, ops, &fakeVault{values: map[string]string{}}, nil)
	preview, err := manager.PreviewGitOpsController(context.Background(), GitOpsRequest{GitOpsScope: scope, Action: "protect-ownership"})
	if err != nil {
		t.Fatal(err)
	}
	nativeGitOpsOperation(t, manager, scope, preview, true)
	direct := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "synthetic-direct"}, Data: map[string]string{"value": "synthetic"}}
	current, err := core.CoreV1().ConfigMaps(scope.Namespace).Create(context.Background(), direct, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim := current.DeepCopy()
	claim.Labels = map[string]string{"kustomize.toolkit.fluxcd.io/name": "synthetic-app", "kustomize.toolkit.fluxcd.io/namespace": scope.Namespace}
	if _, err := core.CoreV1().ConfigMaps(scope.Namespace).Update(context.Background(), claim, metav1.UpdateOptions{}); !(apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) || !strings.Contains(err.Error(), "requires a staged handoff") {
		t.Fatalf("unstaged native ownership adoption was not denied: %v", err)
	}
	current.Annotations = map[string]string{gitOpsHandoff: "synthetic-handoff", gitOpsHandoffTarget: "flux/" + scope.Namespace + "/synthetic-app"}
	current, err = core.CoreV1().ConfigMaps(scope.Namespace).Update(context.Background(), current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Labels = claim.Labels
	current, err = core.CoreV1().ConfigMaps(scope.Namespace).Update(context.Background(), current, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("matching staged native adoption was rejected: %v", err)
	}
	current.Labels["kustomize.toolkit.fluxcd.io/name"] = "competing-app"
	if _, err := core.CoreV1().ConfigMaps(scope.Namespace).Update(context.Background(), current, metav1.UpdateOptions{}); !(apierrors.IsForbidden(err) || apierrors.IsInvalid(err)) || !strings.Contains(err.Error(), "before changing native ownership") {
		t.Fatalf("competing native reconciler was not denied: %v", err)
	}
}
