package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxCleanupOutput = 32 * 1024

// CleanupUnused removes unused runtime data in dependency order: stopped
// standalone containers first, so the build cache, image and anonymous-volume
// passes can release what they referenced, then trims the engine's host disk.
// It shares the runtime read gate with ordinary Docker operations, which keep
// working; every removal refuses objects that gained a reference meanwhile.
func (m *Manager) CleanupUnused(ctx context.Context) (app.DockerCleanupResult, error) {
	result := app.NewDockerCleanupResult()
	ctx, cancel := context.WithTimeout(ctx, app.DockerCleanupTimeout)
	defer cancel()
	transaction, release, err := m.dataReadGate(ctx)
	if err != nil {
		return result, err
	}
	releaseTransaction := sync.OnceFunc(release)
	defer releaseTransaction()
	if m.nativeGuard != nil {
		if err := m.nativeGuard("image", "*"); err != nil {
			return result, err
		}
	}
	if !m.cleanupMu.TryLock() {
		return result, fmt.Errorf("%w: Porto runtime cleanup is already running", ErrConflict)
	}
	defer m.cleanupMu.Unlock()
	backend, err := m.backend(transaction)
	if err != nil {
		return result, err
	}
	connection, err := newBuildKitControlConnection(func(dialContext context.Context) (net.Conn, error) {
		return m.dialBuildKitBackend(dialContext, backend)
	})
	if err != nil {
		return result, fmt.Errorf("create BuildKit cleanup client: %w", err)
	}
	defer connection.Close()
	client := controlapi.NewControlClient(connection)
	probeContext, cancelProbe := context.WithTimeout(transaction, 20*time.Second)
	err = ensureBuildKitIdle(probeContext, client)
	cancelProbe()
	if err != nil {
		return result, err
	}
	disk := m.measureHostDisk(ctx, backend)
	runtimeErr := m.pruneUnusedRuntime(transaction, client, &result)
	// Trimming only touches the VM's free blocks, so storage transactions may resume.
	releaseTransaction()
	if ctx.Err() != nil {
		return result, errors.Join(runtimeErr, context.Cause(ctx))
	}
	var diskErr error
	result.HostDisk, diskErr = m.reclaimHostDisk(ctx, disk)
	return result, errors.Join(runtimeErr, diskErr)
}

func (m *Manager) pruneUnusedRuntime(ctx context.Context, client controlapi.ControlClient, result *app.DockerCleanupResult) error {
	var containersErr error
	result.Containers, containersErr = m.pruneStoppedContainers(ctx, time.Now())
	if ctx.Err() != nil {
		return errors.Join(containersErr, context.Cause(ctx))
	}
	var cacheErr error
	result.BuildCache, cacheErr = pruneBuildCache(ctx, client)
	if ctx.Err() != nil || status.Code(cacheErr) == codes.Canceled || status.Code(cacheErr) == codes.DeadlineExceeded {
		return errors.Join(containersErr, cacheErr, context.Cause(ctx))
	}
	var imageErr error
	result.Images, imageErr = m.pruneUnusedImages(ctx, time.Now())
	if ctx.Err() != nil {
		return errors.Join(containersErr, cacheErr, imageErr, context.Cause(ctx))
	}
	var volumesErr error
	result.Volumes, volumesErr = m.pruneAnonymousVolumes(ctx)
	return errors.Join(containersErr, cacheErr, imageErr, volumesErr)
}

func pruneBuildCache(ctx context.Context, client controlapi.ControlClient) (result app.DockerCleanupStep, err error) {
	result.Status = app.CleanupFailed
	defer func() {
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Status = app.CleanupSucceeded
		}
	}()
	stream, err := client.Prune(ctx, &controlapi.PruneRequest{All: true})
	if err != nil {
		return result, fmt.Errorf("prune Porto build cache: %w", err)
	}
	var reclaimed int64
	knownSize := true
	for {
		record, receiveErr := stream.Recv()
		if receiveErr == io.EOF {
			break
		}
		if receiveErr != nil {
			return result, fmt.Errorf("read Porto build-cache prune results: %w", receiveErr)
		}
		if record.InUse {
			return result, errors.New("BuildKit reported an in-use cache record during unused-cache cleanup")
		}
		result.ItemsRemoved++
		if record.Size < 0 || record.Size > math.MaxInt64-reclaimed {
			knownSize = false
		} else {
			reclaimed += record.Size
		}
		if knownSize {
			result.BytesReclaimed = &reclaimed
		} else {
			result.BytesReclaimed = nil
		}
	}
	if knownSize {
		result.BytesReclaimed = &reclaimed
	}
	return result, nil
}
