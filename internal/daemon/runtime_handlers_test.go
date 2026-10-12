package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/kubernetes"
	"github.com/mbianchidev/porto/internal/ports"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/vm"
)

func TestDockerContainerSnapshotReportsUnavailableInventory(t *testing.T) {
	server := &Server{docker: portodocker.New(nil)}
	response := httptest.NewRecorder()
	server.dockerContainerSnapshot(response, httptest.NewRequest(http.MethodGet, "/api/docker/containers/snapshot", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"available":false`) ||
		!strings.Contains(body, `"containers":[]`) ||
		!strings.Contains(body, `"directInventory":{"supported":true}`) ||
		!strings.Contains(body, `"healthUpdates":{"supported":false`) {
		t.Fatalf("unexpected snapshot response: %s", body)
	}
}

func TestPrepareDockerEngineUpdateAllowsMissingEngine(t *testing.T) {
	server := &Server{
		docker:         portodocker.NewWithStateDir(nil, t.TempDir()),
		runtimeContext: context.Background(),
	}
	mux := http.NewServeMux()
	server.runtimeRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/api/docker/engine/prepare-update", nil),
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"prepared":true`) ||
		!strings.Contains(response.Body.String(), `"engineStopped":false`) {
		t.Fatalf("prepare update response = %d: %s", response.Code, response.Body.String())
	}
}

func TestDockerContainerEventsSendsRevisionedSnapshot(t *testing.T) {
	server := &Server{docker: portodocker.New(nil)}
	response := httptest.NewRecorder()
	server.dockerContainerEvents(response, httptest.NewRequest(http.MethodGet, "/api/docker/containers/events", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}

	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q, want event stream", contentType)
	}
	body := response.Body.String()
	if !strings.Contains(body, "retry: 1000") ||
		!strings.Contains(body, "event: snapshot") ||
		!strings.Contains(body, `"revision":0`) {
		t.Fatalf("unexpected event stream: %s", body)
	}
}

func TestCreateDockerContainerRunsLocalOrRemoteImage(t *testing.T) {
	var commands []string
	runner := runtimeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		args := strings.Join(command.Args, " ")
		commands = append(commands, args)
		switch {
		case strings.HasPrefix(args, "create "):
			return []byte("container-id\n"), nil
		case args == "start container-id":
			return nil, nil
		default:
			return nil, nil
		}
	})
	server := &Server{docker: portodocker.New(runner)}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/docker/containers",
		bytes.NewBufferString(`{
			"name":"web",
			"image":"nginx:alpine",
			"hostPort":8080,
			"containerPort":80,
			"healthCommand":"wget -q -O /dev/null http://127.0.0.1/"
		}`),
	)
	server.createDockerContainer(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create response = %d: %s", response.Code, response.Body.String())
	}
	joined := strings.Join(commands, "\n")
	for _, expected := range []string{
		"create --name web",
		"--health-cmd wget -q -O /dev/null http://127.0.0.1/",
		"--publish 127.0.0.1:8080:80/tcp nginx:alpine",
		"start container-id",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("commands missing %q:\n%s", expected, joined)
		}
	}
}

func TestDockerContainerActionBlocksManagedKindControlPlaneRemoval(t *testing.T) {
	removed := false
	runner := runtimeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "container inspect control-plane-id":
			return []byte(`[{"Id":"control-plane-id","Name":"/porto-dev-control-plane"}]`), nil
		case "rm control-plane-id":
			removed = true
			return nil, nil
		default:
			return nil, nil
		}
	})
	root := t.TempDir()
	metadataPath := filepath.Join(root, config.KubernetesClusterFileToken("dev")+".json")
	if err := os.WriteFile(metadataPath, []byte(`{"name":"dev","provider":"kind"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		docker:   portodocker.New(runner),
		clusters: kubernetes.NewClusterProvisioner(vm.New(runner), runner, root),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/docker/containers/control-plane-id/remove", nil)
	request.SetPathValue("id", "control-plane-id")
	request.SetPathValue("action", "remove")
	response := httptest.NewRecorder()

	server.dockerContainerAction(response, request)

	if response.Code == http.StatusOK || !strings.Contains(response.Body.String(), "control plane for managed Kubernetes cluster dev") {
		t.Fatalf("unexpected removal response %d: %s", response.Code, response.Body.String())
	}
	if removed {
		t.Fatal("managed control plane was removed")
	}
}

func TestKubernetesClusterOperationLockRejectsOverlap(t *testing.T) {
	server := &Server{}
	release, err := server.beginKubernetesClusterOperation(context.Background(), "dev", "stop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.beginKubernetesClusterOperation(context.Background(), "dev", "delete"); err == nil ||
		!strings.Contains(err.Error(), "operation stop is already in progress") {
		t.Fatalf("overlapping cluster operation error = %v", err)
	}
	release()
	releaseAgain, err := server.beginKubernetesClusterOperation(context.Background(), "dev", "delete")
	if err != nil {
		t.Fatalf("cluster operation remained locked: %v", err)
	}
	releaseAgain()
}

func TestKubernetesRenameOperationLocksOldAndNewNames(t *testing.T) {
	server := &Server{}
	release, err := server.beginKubernetesClusterOperation(context.Background(), "prod", "rename", "dev")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := server.beginKubernetesClusterOperation(context.Background(), "dev", "create"); err == nil ||
		!strings.Contains(err.Error(), "operation rename is already in progress") {
		t.Fatalf("create during rename error = %v", err)
	}
}

func TestKubernetesStorageAndGatewayHandlers(t *testing.T) {
	runner := runtimeRunnerFunc(func(_ context.Context, command runtimes.Command) ([]byte, error) {
		if command.Name == "kubectl" && strings.Contains(strings.Join(command.Args, " "), " get ") {
			return []byte(`{"items":[]}`), nil
		}
		return nil, nil
	})
	server := &Server{kubernetes: kubernetes.New(runner)}
	tests := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{"/api/kubernetes/deployments?context=porto-dev&namespace=default", server.kubernetesDeployments},
		{"/api/kubernetes/jobs?context=porto-dev&namespace=default", server.kubernetesJobs},
		{"/api/kubernetes/cronjobs?context=porto-dev&namespace=default", server.kubernetesCronJobs},
		{"/api/kubernetes/persistent-volumes?context=porto-dev", server.kubernetesPersistentVolumes},
		{"/api/kubernetes/persistent-volume-claims?context=porto-dev&namespace=default", server.kubernetesPersistentVolumeClaims},
		{"/api/kubernetes/gateway-classes?context=porto-dev", server.kubernetesGatewayClasses},
		{"/api/kubernetes/gateways?context=porto-dev&namespace=porto-system", server.kubernetesGateways},
		{"/api/kubernetes/http-routes?context=porto-dev&namespace=default", server.kubernetesHTTPRoutes},
	}
	for _, test := range tests {
		response := httptest.NewRecorder()
		test.handler(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != "[]" {
			t.Fatalf("%s response = %d: %s", test.path, response.Code, response.Body.String())
		}
	}
}

func TestKubernetesPortForwardHandlersListAndStop(t *testing.T) {
	devDone := make(chan struct{})
	otherDone := make(chan struct{})
	server := &Server{kubeForwards: map[string]*kubeForward{
		"porto-dev/manual/dev-forward": {
			id:           "dev-forward",
			contextName:  "porto-dev",
			namespace:    "default",
			resourceType: "service",
			resourceName: "api",
			port:         45000,
			remotePort:   8080,
			startedAt:    time.Date(2026, time.September, 12, 12, 0, 0, 0, time.UTC),
			managed:      true,
			done:         devDone,
		},
		"porto-other/manual/other-forward": {
			id:           "other-forward",
			contextName:  "porto-other",
			namespace:    "default",
			resourceType: "pod",
			resourceName: "worker",
			port:         45001,
			remotePort:   3000,
			managed:      true,
			done:         otherDone,
		},
		"porto-dev/gateway": {port: 45002, done: otherDone},
	}}

	listResponse := httptest.NewRecorder()
	server.kubernetesPortForwards(
		listResponse,
		httptest.NewRequest(http.MethodGet, "/api/kubernetes/port-forwards?context=porto-dev", nil),
	)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list response = %d: %s", listResponse.Code, listResponse.Body.String())
	}
	var forwards []kubernetesPortForward
	if err := json.NewDecoder(listResponse.Body).Decode(&forwards); err != nil {
		t.Fatalf("decode forwards: %v", err)
	}
	if len(forwards) != 1 || forwards[0].ID != "dev-forward" || forwards[0].LocalPort != 45000 {
		t.Fatalf("unexpected forwards: %+v", forwards)
	}

	stopRequest := httptest.NewRequest(
		http.MethodDelete,
		"/api/kubernetes/port-forwards/dev-forward?context=porto-dev",
		nil,
	)
	stopRequest.SetPathValue("id", "dev-forward")
	stopResponse := httptest.NewRecorder()
	close(devDone)
	server.stopKubernetesPortForward(stopResponse, stopRequest)
	if stopResponse.Code != http.StatusNoContent {
		t.Fatalf("stop response = %d: %s", stopResponse.Code, stopResponse.Body.String())
	}
	if server.kubeForwards["porto-dev/manual/dev-forward"] != nil {
		t.Fatal("stopped port forward remained registered")
	}
	if server.kubeForwards["porto-other/manual/other-forward"] == nil {
		t.Fatal("port forward from another context was removed")
	}
}

func TestCreateKubernetesPortForwardRejectsUnsupportedResource(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/kubernetes/port-forwards?context=porto-dev",
		bytes.NewBufferString(`{
			"namespace":"default",
			"resourceType":"secret",
			"resourceName":"credentials",
			"remotePort":443
		}`),
	)
	server.createKubernetesPortForward(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "service, pod, or deployment") {
		t.Fatalf("unexpected response = %d: %s", response.Code, response.Body.String())
	}
}

func TestCreateAndStopKubernetesPortForward(t *testing.T) {
	for _, delay := range []time.Duration{0, 2500 * time.Millisecond} {
		t.Run(delay.String(), func(t *testing.T) {
			testCreateAndStopKubernetesPortForward(t, delay)
		})
	}
}

func testCreateAndStopKubernetesPortForward(t *testing.T, delay time.Duration) {
	t.Helper()
	configureKubernetesPortForwardHelper(t, delay)
	localPort := freeKubernetesForwardTestPort(t)

	runContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := &Server{
		kubeForwards:   map[string]*kubeForward{},
		kubernetes:     kubernetes.New(nil),
		runtimeContext: runContext,
	}
	t.Cleanup(func() {
		cancel()
		if err := server.stopKubernetesForwards("porto-dev"); err != nil {
			t.Errorf("clean up port forwards: %v", err)
		}
	})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/kubernetes/port-forwards?context=porto-dev",
		bytes.NewBufferString(fmt.Sprintf(`{
			"namespace":"default",
			"resourceType":"deployment",
			"resourceName":"api",
			"localPort":%d,
			"remotePort":8080
		}`, localPort)),
	)
	server.createKubernetesPortForward(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("create response = %d: %s", response.Code, response.Body.String())
	}
	var created kubernetesPortForward
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("decode created forward: %v", err)
	}
	if created.LocalPort != localPort || created.RemotePort != 8080 ||
		created.ResourceType != "deployment" || created.ResourceName != "api" {
		t.Fatalf("unexpected created forward: %+v", created)
	}

	stopRequest := httptest.NewRequest(
		http.MethodDelete,
		"/api/kubernetes/port-forwards/"+created.ID+"?context=porto-dev",
		nil,
	)
	stopRequest.SetPathValue("id", created.ID)
	stopResponse := httptest.NewRecorder()
	server.stopKubernetesPortForward(stopResponse, stopRequest)
	if stopResponse.Code != http.StatusNoContent {
		t.Fatalf("stop response = %d: %s", stopResponse.Code, stopResponse.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for !ports.IsFree(localPort) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ports.IsFree(localPort) {
		t.Fatalf("local port %d remained occupied", localPort)
	}
}

func TestWaitForKubernetesForwardReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	localPort := listener.Addr().(*net.TCPAddr).Port
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name    string
		ctx     context.Context
		exited  bool
		wantErr string
	}{
		{name: "listening", ctx: context.Background()},
		{name: "exited", ctx: context.Background(), exited: true, wantErr: "exited before listening"},
		{name: "canceled", ctx: canceledContext, wantErr: context.Canceled.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(test.ctx, time.Second)
			defer cancel()
			forward := &kubeForward{port: localPort, done: make(chan struct{})}
			if test.exited {
				close(forward.done)
			}
			err := waitForKubernetesForwardReady(ctx, forward)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("wait for listener: %v", err)
				}
			} else if err == nil || err.Error() != test.wantErr {
				t.Fatalf("startup error = %v, want %q", err, test.wantErr)
			}
		})
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	forward := &kubeForward{port: localPort, done: make(chan struct{})}
	if err := waitForKubernetesForwardReady(ctx, forward); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startup error = %v, want deadline exceeded", err)
	}
}

func TestKubernetesPortForwardStartupCancellationReapsProcess(t *testing.T) {
	configureKubernetesPortForwardHelper(t, time.Hour)
	localPort := freeKubernetesForwardTestPort(t)
	runContext, cancel := context.WithCancel(context.Background())
	server := &Server{
		kubeForwards:   map[string]*kubeForward{},
		kubernetes:     kubernetes.New(nil),
		runtimeContext: runContext,
	}
	t.Cleanup(func() {
		cancel()
		if err := server.stopKubernetesForwards("porto-dev"); err != nil {
			t.Errorf("clean up port forwards: %v", err)
		}
	})
	key := "porto-dev/manual/startup"
	result := make(chan error, 1)
	go func() {
		_, err := server.startKubernetesForward(key, kubernetesForwardSpec{
			contextName:  "porto-dev",
			namespace:    "default",
			resourceType: "deployment",
			resourceName: "api",
			localPort:    localPort,
			remotePort:   8080,
		})
		result <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var forward *kubeForward
	for forward == nil {
		server.mu.Lock()
		forward = server.kubeForwards[key]
		server.mu.Unlock()
		if forward != nil {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("startup returned before cancellation: %v", err)
		case <-deadline.C:
			t.Fatal("port forward process did not start")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup error = %v, want context canceled", err)
		}
	case <-deadline.C:
		t.Fatal("startup did not return after cancellation")
	}
	if !kubernetesForwardDone(forward) {
		t.Fatal("startup returned before the process was reaped")
	}
	server.mu.Lock()
	remaining := server.kubeForwards[key]
	server.mu.Unlock()
	if remaining != nil {
		t.Fatal("canceled port forward remained registered")
	}
	if !ports.IsFree(localPort) {
		t.Fatalf("local port %d remained occupied", localPort)
	}
}

func freeKubernetesForwardTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release local port: %v", err)
	}
	return localPort
}

func configureKubernetesPortForwardHelper(t *testing.T, delay time.Duration) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("kubectl helper wrapper is Unix-specific")
	}
	binDir := t.TempDir()
	kubectlPath := filepath.Join(binDir, "kubectl")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestKubernetesPortForwardHelperProcess$ -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(kubectlPath, []byte(script), 0o700); err != nil {
		t.Fatalf("write kubectl helper: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PORTO_KUBERNETES_FORWARD_HELPER", "1")
	t.Setenv("PORTO_KUBERNETES_FORWARD_HELPER_DELAY", delay.String())
}

func TestKubernetesPortForwardHelperProcess(t *testing.T) {
	if os.Getenv("PORTO_KUBERNETES_FORWARD_HELPER") != "1" {
		return
	}
	delay, err := time.ParseDuration(os.Getenv("PORTO_KUBERNETES_FORWARD_HELPER_DELAY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid synthetic kubectl startup delay: %v\n", err)
		os.Exit(20)
	}
	time.Sleep(delay)
	mapping := os.Args[len(os.Args)-1]
	localPort, _, ok := strings.Cut(mapping, ":")
	if !ok {
		os.Exit(21)
	}
	port, err := strconv.Atoi(localPort)
	if err != nil {
		os.Exit(22)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		os.Exit(23)
	}
	defer listener.Close()
	for {
		connection, err := listener.Accept()
		if err != nil {
			os.Exit(24)
		}
		_ = connection.Close()
	}
}
