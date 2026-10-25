package daemon

import (
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func TestContainerHTTPRoutesIncludeNativeBackendAndRejectCrossBackendClaims(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	snapshot := func(id, name string, port int32) portodocker.ContainerSnapshot {
		return portodocker.ContainerSnapshot{Available: true, Containers: []portodocker.Container{{
			ID: id, Name: name, State: "running", TaskPresent: true,
			Labels:         map[string]string{"dev.porto.http-port": "8080"},
			NetworkDetails: []portodocker.ContainerNetworkState{{ContainerPort: 8080, HostPort: port, HostIP: "127.0.0.1", Protocol: "tcp"}},
		}}}
	}
	linux, native := snapshot("linux-id", "linux-app", 41001), snapshot("native-id", "windows-app", 41002)
	routes, _ := backendHTTPRoutes(linux, native)
	if len(routes) != 2 || routes["windows-app.docker"].Backend != "windows" ||
		routes["linux-app.docker"].Backend != "linux" {
		t.Fatalf("backend routing lost origin: %+v", routes)
	}
	native.Containers[0].Name = "linux-app"
	native.Containers[0].ID = "linux-id"
	routes, warnings := backendHTTPRoutes(linux, native)
	if len(routes) != 0 || len(warnings) == 0 {
		t.Fatalf("cross-backend hostname collision was routed: %+v %v", routes, warnings)
	}
	native.Stale = true
	routes, _ = backendHTTPRoutes(linux, native)
	if len(routes) != 1 || routes["linux-app.docker"].Backend != "linux" {
		t.Fatalf("stale native inventory replaced a valid Linux route: %+v", routes)
	}
}
