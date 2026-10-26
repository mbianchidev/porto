package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/resourcesaver"
)

func TestResourceSaverDoesNotTreatRuntimeSystemMountsAsUserState(t *testing.T) {
	system := []portodocker.ContainerMount{
		{Type: "proc", Destination: "/proc"},
		{Type: "tmpfs", Destination: "/dev"},
		{Type: "sysfs", Destination: "/sys"},
		{Type: "bind", Destination: "/etc/hosts"},
		{Type: "bind", Destination: "/etc/hostname"},
		{Type: "bind", Destination: "/etc/resolv.conf"},
	}

	if saverHasStatefulMounts(system) {
		t.Fatal("standard OCI system mounts made every stateless service ineligible")
	}
	for _, mount := range []portodocker.ContainerMount{
		{Type: "volume", Destination: "/data"},
		{Type: "bind", Destination: "/workspace"},
	} {
		if !saverHasStatefulMounts(append(system, mount)) {
			t.Fatal("mounted application data did not require separate opt-in")
		}
	}
}

type unreadySaverBackend struct{}

func (unreadySaverBackend) Snapshot(context.Context) (resourcesaver.Snapshot, error) {
	return resourcesaver.Snapshot{}, nil
}
func (unreadySaverBackend) Sleep(context.Context, resourcesaver.Entry) error  { return nil }
func (unreadySaverBackend) Resume(context.Context, resourcesaver.Entry) error { return nil }
func (unreadySaverBackend) Ready(context.Context, resourcesaver.Application) (bool, error) {
	return false, nil
}
func (unreadySaverBackend) SuspendEngine(context.Context, string) error { return nil }
func (unreadySaverBackend) WakeEngine(context.Context, string) error    { return nil }

func TestCanceledHTTPUploadReapsItsWakeBeforeReadinessDeadline(t *testing.T) {
	configuration := resourcesaver.Defaults()
	configuration.Mode, configuration.WakeSeconds = resourcesaver.ScaleZero, 30
	application := resourcesaver.Application{
		ID: "synthetic-application", Members: []resourcesaver.Member{{ID: "synthetic-container", Identity: "synthetic-identity"}},
		Routes: []resourcesaver.Route{{Hostname: "synthetic.docker", ContainerID: "synthetic-container", Host: "127.0.0.1", Port: 12345}},
	}
	state := resourcesaver.State{Config: configuration, Entries: map[string]resourcesaver.Entry{
		application.ID: {Application: application, Mode: resourcesaver.ScaleZero, Token: "synthetic-token", Phase: "sleeping"},
	}}
	path := filepath.Join(t.TempDir(), "saver.json")
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	manager, err := resourcesaver.New(ctx, path, unreadySaverBackend{})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{resourceSaver: manager, networkRoutes: map[string]containerHTTPRoute{
		"synthetic.docker": {ContainerID: "synthetic-container", Host: "127.0.0.1", Port: 12345},
	}}
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		server.proxyContainerRoute(w, request, "synthetic.docker")
	}))
	t.Cleanup(func() {
		stop()
		if err := manager.Close(context.Background()); err != nil {
			t.Error(err)
		}
		listener.Close()
	})
	requestContext, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, listener.URL, strings.NewReader("synthetic initiating body"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()
	waitPolicy(t, ctx, func() bool { return manager.State().Pending == 1 }, "real initiating upload waits for readiness")
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("real client cancellation returned %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for manager.State().Pending != 0 {
		if time.Now().After(deadline) {
			t.Fatal("canceling a real HTTP upload left its wake reserved until the readiness deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
