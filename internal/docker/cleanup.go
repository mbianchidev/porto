package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxCleanupOutput = 32 * 1024

var imagePrunePlatformWarning = regexp.MustCompile(`^(?:time="[^"]+" )?level=warning msg="failed to enumerate rootfs" error="no match for platform in manifest sha256:[0-9a-f]{64}: not found"$`)

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
	if _, ok := m.runner.(streamingRunner); !ok {
		return result, fmt.Errorf("%w: image cleanup requires separate stdout/stderr capture", ErrUnsupported)
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
	runtimeErr := m.pruneUnusedRuntime(transaction, backend, client, &result)
	// Trimming only touches the VM's free blocks, so storage transactions may resume.
	releaseTransaction()
	if ctx.Err() != nil {
		return result, errors.Join(runtimeErr, context.Cause(ctx))
	}
	var diskErr error
	result.HostDisk, diskErr = m.reclaimHostDisk(ctx, disk)
	return result, errors.Join(runtimeErr, diskErr)
}

func (m *Manager) pruneUnusedRuntime(ctx context.Context, backend commandBackend, client controlapi.ControlClient, result *app.DockerCleanupResult) error {
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
	result.Images, imageErr = m.pruneUnusedImages(ctx, backend, time.Now())
	if ctx.Err() != nil {
		return errors.Join(containersErr, cacheErr, imageErr, context.Cause(ctx))
	}
	var volumesErr error
	result.Volumes, volumesErr = m.pruneAnonymousVolumes(ctx)
	return errors.Join(containersErr, cacheErr, imageErr, volumesErr)
}

// pruneUnusedImages removes image references no container uses by name or
// ID. Unlike `nerdctl image prune`, it keeps Porto checkpoint images, which no
// container names, and references created in the last minimum-age window.
// `nerdctl image rm` without --force still refuses an image that gained a user.
func (m *Manager) pruneUnusedImages(ctx context.Context, backend commandBackend, now time.Time) (app.DockerCleanupStep, error) {
	step := app.DockerCleanupStep{Status: app.CleanupFailed}
	images, err := m.imagesWithTimeout(ctx, max(m.timeout, imageTransferTimeout))
	if err != nil {
		err = fmt.Errorf("list images for cleanup: %w", err)
		step.Error = err.Error()
		return step, err
	}
	containers, err := m.storageContainers(ctx)
	if err != nil {
		err = fmt.Errorf("list image references for cleanup: %w", err)
		step.Error = err.Error()
		return step, err
	}
	cutoff := now.Add(-app.DockerCleanupMinimumAge)
	candidates := make([]string, 0)
	listed := make(map[string]bool)
	kept := 0
	for _, image := range images {
		if image.Name == "" || listed[image.Name] {
			continue
		}
		listed[image.Name] = true
		created := parseDockerTimestamp(image.CreatedAt)
		if strings.HasPrefix(image.Name, checkpointImagePrefix) || created.IsZero() || !created.Before(cutoff) ||
			slices.ContainsFunc(containers, func(container Container) bool { return containerUsesImage(container, image) }) {
			kept++
			continue
		}
		candidates = append(candidates, image.Name)
	}
	var output imagePruneOutput
	if kept > 0 {
		output.append(&output.stdout, fmt.Sprintf("Kept %d image reference(s): used by a container, holding a Porto checkpoint, or created less than %s ago.\n", kept, app.DockerCleanupMinimumAge))
	}
	var runErr error
	if len(candidates) > 0 {
		if backend.limaInstance != "" {
			backend.prefix = []string{"shell", "--workdir=/", backend.limaInstance, "--", "nerdctl"}
		}
		runErr = m.runBackendStreamingInput(
			ctx, backend, app.DockerCleanupTimeout, "remove unused Porto images", nil, nil, output.write,
			append([]string{"image", "rm"}, candidates...)...,
		)
	}
	return output.result(runErr)
}

func containerUsesImage(container Container, image Image) bool {
	for _, reference := range []string{container.Image, container.ImageID, container.Labels[nerdctlImageDigestLabel]} {
		if imageMatchesIdentifier(image, reference) || reference != "" && imageMatchesIdentifier(image, normalizeNerdctlReference(reference)) {
			return true
		}
	}
	return false
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

type imagePruneOutput struct {
	mu                sync.Mutex
	stdout            strings.Builder
	stderr            strings.Builder
	pending           string
	diagnosticPending string
	removed           int64
	parseErr          error
	diagnosticErr     error
	truncated         bool
}

func (o *imagePruneOutput) write(chunk runtimes.OutputChunk) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	output, pending, parseLine := &o.stdout, &o.pending, o.parseLine
	switch chunk.Stream {
	case "stdout":
	case "stderr":
		output, pending, parseLine = &o.stderr, &o.diagnosticPending, o.parseDiagnosticLine
	default:
		return fmt.Errorf("unknown image prune output stream %q", chunk.Stream)
	}
	o.append(output, string(chunk.Data))
	lines := strings.Split(*pending+string(chunk.Data), "\n")
	for _, line := range lines[:len(lines)-1] {
		parseLine(line)
	}
	*pending = lines[len(lines)-1]
	if len(*pending) > maxCleanupOutput {
		return fmt.Errorf("image prune %s line exceeds the result limit", chunk.Stream)
	}
	return nil
}

func (o *imagePruneOutput) append(output *strings.Builder, data string) {
	remaining := maxCleanupOutput - output.Len()
	if len(data) > remaining {
		o.truncated = true
		data = data[:remaining]
	}
	output.WriteString(data)
}

func (o *imagePruneOutput) parseLine(line string) {
	line = strings.TrimSpace(line)
	lower := strings.ToLower(line)
	switch {
	case strings.HasPrefix(lower, "untagged: ") && strings.TrimSpace(line[len("untagged: "):]) != "":
		o.removed++
	case line == "", lower == "deleted images:", strings.HasPrefix(lower, "deleted: "), strings.HasPrefix(lower, "total reclaimed space:"):
	default:
		if o.parseErr == nil {
			o.parseErr = fmt.Errorf("unexpected image prune output: %.512s", line)
		}
	}
}

func (o *imagePruneOutput) parseDiagnosticLine(line string) {
	line = strings.TrimSpace(line)
	// nerdctl uses the host-platform rootfs only to print deleted layer digests.
	if line == "" || imagePrunePlatformWarning.MatchString(line) {
		return
	}
	if o.diagnosticErr == nil {
		o.diagnosticErr = fmt.Errorf("image cleanup reported runtime diagnostics: %.512s", line)
	}
}

func (o *imagePruneOutput) result(runErr error) (app.DockerCleanupStep, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parseLine(o.pending)
	o.parseDiagnosticLine(o.diagnosticPending)
	output := strings.TrimSpace(o.stdout.String())
	diagnostics := strings.TrimSpace(o.stderr.String())
	if diagnostics != "" {
		output = strings.TrimSpace(output + "\n" + diagnostics)
	}
	if o.truncated {
		output += "\n[Runtime output truncated; reported removal counts include all parsed lines.]"
	}
	err := errors.Join(runErr, o.parseErr, o.diagnosticErr)
	result := app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: o.removed, Output: output}
	if err != nil {
		result.Status = app.CleanupFailed
		result.Error = err.Error()
	}
	return result, err
}
