package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbianchidev/porto/internal/autoscaling"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/store"
)

type fakeAutoscalingService struct {
	configured  autoscaling.Config
	workload    autoscaling.ScaledObject
	deleted     []string
	paused      []string
	resumed     []string
	onPause     func() error
	onConfigure func() error
}

func (f *fakeAutoscalingService) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (f *fakeAutoscalingService) Configure(_ context.Context, name string, cfg autoscaling.Config) (autoscaling.Status, error) {
	f.configured = cfg
	if f.onConfigure != nil {
		if err := f.onConfigure(); err != nil {
			return autoscaling.Status{}, err
		}
	}
	return autoscaling.Status{Config: cfg, Cluster: name, NodeSupported: true}, nil
}
func (f *fakeAutoscalingService) Status(_ context.Context, name string) (autoscaling.Status, error) {
	return autoscaling.Status{Config: f.configured, Cluster: name, NodeSupported: true}, nil
}
func (f *fakeAutoscalingService) Pause(_ context.Context, name string) error {
	f.paused = append(f.paused, name)
	if f.onPause != nil {
		return f.onPause()
	}
	return nil
}
func (f *fakeAutoscalingService) Resume(_ context.Context, name string) error {
	f.resumed = append(f.resumed, name)
	return nil
}
func (*fakeAutoscalingService) Forget(context.Context, string) error         { return nil }
func (*fakeAutoscalingService) Rename(context.Context, string, string) error { return nil }
func (f *fakeAutoscalingService) ApplyScaledObject(_ context.Context, _ string, spec autoscaling.ScaledObject) error {
	f.workload = spec
	return nil
}
func (f *fakeAutoscalingService) DeleteScaledObject(_ context.Context, cluster, namespace, name string) error {
	f.deleted = []string{cluster, namespace, name}
	return nil
}

func TestAutoscalingConfigureOwnsItsOperationGate(t *testing.T) {
	service := &fakeAutoscalingService{}
	server := &Server{autoscaling: service, clusters: kubernetes.NewClusterProvisioner(nil, nil, t.TempDir())}
	service.onConfigure = func() error {
		release, err := server.beginKubernetesClusterOperation(context.Background(), "fixture", "autoscaling-configure")
		if err != nil {
			return err
		}
		release()
		release() // Explicit completion and deferred release must be safe together.
		return nil
	}
	request := httptest.NewRequest(http.MethodPost, "/api/kubernetes/clusters/fixture/autoscaling", strings.NewReader(`{"nodes":{"enabled":true,"maxNodes":2,"machine":{"cpus":2,"memoryMiB":2048,"diskGiB":20}},"keda":true}`))
	request.SetPathValue("name", "fixture")
	response := httptest.NewRecorder()
	server.configureKubernetesAutoscaling(response, request)
	if response.Code != http.StatusOK || !service.configured.KEDA || service.configured.Nodes.MaxNodes != 2 {
		t.Fatalf("response = %d: %s, config %+v", response.Code, response.Body.String(), service.configured)
	}
}

func TestClusterLifecyclePausesAutoscalingBeforeGate(t *testing.T) {
	service := &fakeAutoscalingService{}
	server := &Server{autoscaling: service, clusters: kubernetes.NewClusterProvisioner(nil, nil, t.TempDir())}
	service.onPause = func() error {
		release, err := server.beginKubernetesClusterOperation(context.Background(), "fixture", "autoscaling-inflight")
		if err == nil {
			release()
		}
		return err
	}
	release, err := server.beginKubernetesClusterOperation(context.Background(), "fixture", "upgrade")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(service.paused) != 1 || service.paused[0] != "fixture" {
		t.Fatalf("pause calls = %v", service.paused)
	}
}

func TestWorkloadScalingAPIRequiresConfirmationForRemoval(t *testing.T) {
	service := &fakeAutoscalingService{}
	server := &Server{autoscaling: service}
	request := httptest.NewRequest(http.MethodDelete, "/api/kubernetes/clusters/fixture/autoscaling/workloads/default/fixture", nil)
	request.SetPathValue("name", "fixture")
	request.SetPathValue("namespace", "default")
	request.SetPathValue("scaler", "fixture")
	response := httptest.NewRecorder()
	server.deleteKubernetesWorkloadScaling(response, request)
	if response.Code != http.StatusBadRequest || len(service.deleted) != 0 {
		t.Fatalf("unconfirmed delete = %d, %v", response.Code, service.deleted)
	}
	request.URL.RawQuery = "confirm=true"
	response = httptest.NewRecorder()
	server.deleteKubernetesWorkloadScaling(response, request)
	if response.Code != http.StatusNoContent || strings.Join(service.deleted, "/") != "fixture/default/fixture" {
		t.Fatalf("confirmed delete = %d, %v", response.Code, service.deleted)
	}
}

func TestDisablingKubernetesPausesAutoscaling(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	settings, err := st.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	settings.KubernetesEnabled = true
	if err := st.SetSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	p := kubernetes.NewClusterProvisioner(nil, nil, root)
	if err := os.WriteFile(filepath.Join(root, config.KubernetesClusterFileToken("fixture")+".json"), []byte(`{"name":"fixture","provider":"k3s"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &fakeAutoscalingService{}
	server := &Server{store: st, clusters: p, autoscaling: service}
	request := httptest.NewRequest(http.MethodPost, "/api/runtime/features/kubernetes/disable", nil)
	request.SetPathValue("feature", "kubernetes")
	request.SetPathValue("action", "disable")
	response := httptest.NewRecorder()
	server.setRuntimeFeature(response, request)
	if response.Code != http.StatusOK || len(service.paused) != 1 {
		t.Fatalf("runtime disable = %d: %s, pauses %v", response.Code, response.Body.String(), service.paused)
	}
}
