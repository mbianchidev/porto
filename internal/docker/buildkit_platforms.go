package docker

import (
	"context"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/containerd/platforms"
	controlapi "github.com/moby/buildkit/api/services/control"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
)

func (m *Manager) refreshLimaBuildKitPlatforms(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	missing, err := m.limaBuildKitMissingPlatforms(ctx, true)
	if err != nil || len(missing) == 0 {
		return err
	}
	if _, err := m.runCommand(
		ctx,
		30*time.Second,
		"refresh Porto BuildKit platforms",
		nil,
		"limactl", "shell", "--workdir=/", engineInstanceName, "--",
		"systemctl", "--user", "restart", "default-buildkit.service",
	); err != nil {
		return err
	}
	missing, err = m.limaBuildKitMissingPlatforms(ctx, false)
	if err != nil {
		return err
	}
	if len(missing) != 0 {
		return fmt.Errorf("BuildKit still does not report configured platforms after refresh: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (m *Manager) limaBuildKitMissingPlatforms(ctx context.Context, requireIdle bool) ([]string, error) {
	connection, err := newBuildKitControlConnection(func(dialContext context.Context) (net.Conn, error) {
		return m.dialBuildKitBackend(dialContext, commandBackend{name: "limactl", limaInstance: engineInstanceName})
	})
	if err != nil {
		return nil, fmt.Errorf("create BuildKit platform client: %w", err)
	}
	defer connection.Close()
	client := controlapi.NewControlClient(connection)
	workers, err := client.ListWorkers(ctx, &controlapi.ListWorkersRequest{}, grpc.WaitForReady(true))
	if err != nil {
		return nil, fmt.Errorf("inspect BuildKit platforms: %w", err)
	}
	if len(workers.Record) == 0 {
		return nil, fmt.Errorf("%w: BuildKit returned no workers", ErrUnavailable)
	}
	// BuildKit's hot refresh can hide new ARM32/i386 support behind native 64-bit platforms.
	required := map[string]struct{}{
		"linux/arm/v7": {},
		"linux/arm/v6": {},
		"linux/386":    {},
	}
	for _, worker := range workers.Record {
		for _, platform := range worker.GetPlatforms() {
			if platform == nil {
				continue
			}
			value := platforms.Normalize(ocispec.Platform{
				OS: platform.OS, Architecture: platform.Architecture, Variant: platform.Variant,
			})
			delete(required, platforms.Format(value))
		}
	}
	missing := slices.Sorted(maps.Keys(required))
	if len(missing) == 0 || !requireIdle {
		return missing, nil
	}
	if err := ensureBuildKitIdle(ctx, client); err != nil {
		return nil, fmt.Errorf("check builds before refreshing BuildKit platforms: %w", err)
	}
	return missing, nil
}
