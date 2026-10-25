package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/networking/networktest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNativeNetworkingTransferUsesResolvedPublicCABytesAndPrivateTransport(t *testing.T) {
	fixture := networktest.New(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	policy := networking.New(t.TempDir())
	certificate, err := policy.ImportCertificate(fixture.CA)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(networking.Config{Mode: "manual", HTTPSProxy: fixture.Proxy, RegistryCAs: []string{certificate.Reference}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{networking: policy, windowsTransport: nativeRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "localhost" || request.URL.Path != "/api/worker/networking" || request.Method != http.MethodPost {
			t.Fatalf("network transfer escaped private worker: %s %s", request.Method, request.URL)
		}
		var transferred networking.RuntimeConfiguration
		if err := json.NewDecoder(request.Body).Decode(&transferred); err != nil {
			t.Fatal(err)
		}
		if len(transferred.Config.RegistryCAs) != 0 || string(transferred.CAPEM) == "" || transferred.Config.HTTPSProxy != fixture.Proxy {
			t.Fatalf("worker was given host references instead of resolved networking: %+v", transferred.Config)
		}
		worker, err := networking.NewRuntime(t.TempDir(), transferred)
		if err != nil {
			t.Fatal(err)
		}
		transport, err := worker.Transport()
		if err != nil {
			t.Fatal(err)
		}
		defer transport.CloseIdleConnections()
		response, err := (&http.Client{Transport: transport}).Get(fixture.URL + "/v2/")
		if err != nil {
			t.Fatalf("transferred native policy could not reach its trusted proxy registry: %v", err)
		}
		response.Body.Close()
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if applied, err := server.syncNativeNetworking(context.Background()); err != nil || !applied {
		t.Fatalf("private owner-to-worker policy apply: %t %v", applied, err)
	}
	if fixture.ProxyHits.Load() == 0 {
		t.Fatal("transferred policy bypassed its proxy")
	}
}

func TestNativeHTTPRouteRevalidatesCurrentWorkerIdentityBeforeProxying(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "synthetic-native-route")
	}))
	defer upstream.Close()
	_, portValue, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	port, err := strconv.Atoi(portValue)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := portodocker.ContainerSnapshot{Available: true, Containers: []portodocker.Container{{
		ID: "owned-native", Name: "native-app", TaskPresent: true, State: "running",
		Labels:         map[string]string{"dev.porto.http-port": "8080"},
		NetworkDetails: []portodocker.ContainerNetworkState{{HostIP: "127.0.0.1", HostPort: int32(port), ContainerPort: 8080, Protocol: "tcp"}},
	}}}
	server := &Server{docker: portodocker.NewWithStateDir(nil, t.TempDir()),
		windowsTransport: nativeRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/api/docker/containers/snapshot" {
				t.Fatalf("unexpected native route probe %s", request.URL)
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
		})}
	server.networkRoutes, _ = backendHTTPRoutes(portodocker.ContainerSnapshot{}, snapshot)
	read := func(want int) {
		t.Helper()
		response := httptest.NewRecorder()
		if !server.proxyContainerRoute(response, httptest.NewRequest(http.MethodGet, "https://native-app.docker.porto.localhost/", nil), "native-app.docker") ||
			response.Code != want {
			t.Fatalf("native HTTP route response: HTTP %d %s", response.Code, response.Body.String())
		}
	}
	read(http.StatusOK)
	snapshot.Containers[0].ID = "recreated-native"
	read(http.StatusServiceUnavailable)
	snapshot.Stale = true
	read(http.StatusServiceUnavailable)
}
