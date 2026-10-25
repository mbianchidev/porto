package daemon

import (
	"testing"

	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func TestContainerHTTPRoutesRequireOptInAndOwnedLiveLoopbackMapping(t *testing.T) {
	t.Setenv("PORTO_HOME", t.TempDir())
	snapshot := portodocker.ContainerSnapshot{Available: true, Containers: []portodocker.Container{
		{ID: "synthetic-api", Name: "api", State: "running", TaskPresent: true,
			Labels:         map[string]string{"dev.porto.http-port": "8080"},
			NetworkDetails: []portodocker.ContainerNetworkState{{ContainerPort: 8080, HostPort: 41001, HostIP: "127.0.0.1", Protocol: "tcp"}}},
		{ID: "synthetic-private", Name: "private", State: "running", TaskPresent: true,
			NetworkDetails: []portodocker.ContainerNetworkState{{ContainerPort: 8080, HostPort: 41002}}},
		{ID: "synthetic-stopped", Name: "stopped", State: "created",
			Labels:         map[string]string{"dev.porto.http-port": "8080"},
			NetworkDetails: []portodocker.ContainerNetworkState{{ContainerPort: 8080, HostPort: 41003}}},
	}}
	routes, warnings := containerHTTPRoutes(snapshot)
	if len(warnings) != 0 || len(routes) != 1 {
		t.Fatalf("HTTP routes = %+v, warnings=%v", routes, warnings)
	}
	if route := routes["api.docker"]; route.ContainerID != "synthetic-api" || route.Port != 41001 || route.Host != "127.0.0.1" {
		t.Fatalf("route = %+v", route)
	}
	snapshot.Stale = true
	routes, _ = containerHTTPRoutes(snapshot)
	if len(routes) != 0 {
		t.Fatal("stale inventory exposed an old listener")
	}
}
