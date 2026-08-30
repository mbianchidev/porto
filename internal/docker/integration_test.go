package docker

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDockerReadOnlyIntegration(t *testing.T) {
	if os.Getenv("PORTO_DOCKER_INTEGRATION") != "1" {
		t.Skip("set PORTO_DOCKER_INTEGRATION=1 to test a live Docker endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager := New(nil)
	status := manager.Status(ctx, "")
	if !status.Available {
		t.Fatalf("Docker unavailable: %s", status.Message)
	}
	if _, err := manager.Containers(ctx); err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if _, err := manager.Images(ctx); err != nil {
		t.Fatalf("list images: %v", err)
	}
	if _, err := manager.Networks(ctx); err != nil {
		t.Fatalf("list networks: %v", err)
	}
	if _, err := manager.Volumes(ctx); err != nil {
		t.Fatalf("list volumes: %v", err)
	}
}
