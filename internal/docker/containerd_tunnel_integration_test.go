package docker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

func TestLimaContainerdProductionConnectionIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_TUNNEL_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_TUNNEL_INTEGRATION=1 to test the production connector against an already running Porto-owned Lima engine")
	}
	manager := New(nil)
	state, err := manager.readEngineState()
	if err != nil || state.Mode != "lima" || state.Instance != engineInstanceName {
		t.Fatalf("an existing Porto-owned Lima engine is required; none was started: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exists, running, err := manager.limaInstanceStatus(ctx)
	if err != nil || !exists || !running {
		t.Fatalf("an already running engine is required: exists=%v running=%v error=%v", exists, running, err)
	}
	if err := manager.verifyLimaOwnership(ctx, state.OwnerID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		dialContext, stopDial := context.WithTimeout(ctx, 20*time.Second)
		client, err := manager.connectContainerRuntime(dialContext)
		stopDial()
		if err != nil {
			t.Fatalf("production containerd connection: %v", err)
		}
		backend, ok := client.(*grpcContainerRuntime)
		if !ok {
			t.Fatal(errors.Join(errors.New("production connector did not return a containerd client"), client.Close()))
		}
		probe, err := grpc_health_v1.NewHealthClient(backend.connection).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
		closeErr := client.Close()
		if err != nil || closeErr != nil {
			t.Fatal(errors.Join(err, closeErr))
		}
		if probe.GetStatus() != grpc_health_v1.HealthCheckResponse_SERVING {
			t.Fatalf("containerd health after dial cancellation: %s", probe.GetStatus())
		}
		if _, err := manager.runCommand(ctx, 10*time.Second, "verify shared Lima shell after tunnel close", nil,
			"limactl", "shell", "--workdir=/", state.Instance, "--", "true"); err != nil {
			t.Fatalf("closing the production connection disturbed the shared Lima SSH channel: %v", err)
		}
	}
}
