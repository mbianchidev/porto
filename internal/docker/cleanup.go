package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
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

func (m *Manager) CleanupUnused(ctx context.Context) (app.DockerCleanupResult, error) {
	result := app.NewDockerCleanupResult()
	if !m.cleanupMu.TryLock() {
		return result, fmt.Errorf("%w: Porto runtime cleanup is already running", ErrConflict)
	}
	defer m.cleanupMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, app.DockerCleanupTimeout)
	defer cancel()
	backend, err := m.backend(ctx)
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
	probeContext, cancelProbe := context.WithTimeout(ctx, 20*time.Second)
	err = ensureBuildKitIdle(probeContext, client)
	cancelProbe()
	if err != nil {
		return result, err
	}
	var cacheErr error
	result.BuildCache, cacheErr = pruneBuildCache(ctx, client)
	if ctx.Err() != nil || status.Code(cacheErr) == codes.Canceled || status.Code(cacheErr) == codes.DeadlineExceeded {
		return result, errors.Join(cacheErr, context.Cause(ctx))
	}
	if backend.limaInstance != "" {
		backend.prefix = []string{"shell", "--workdir=/", backend.limaInstance, "--", "nerdctl"}
	}
	var output imagePruneOutput
	imageErr := m.runBackendStreamingInput(
		ctx, backend, app.DockerCleanupTimeout, "prune unused Porto images", nil, nil, output.write,
		"image", "prune", "--all", "--force",
	)
	result.Images, imageErr = output.result(imageErr)
	return result, errors.Join(cacheErr, imageErr)
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
	mu        sync.Mutex
	stdout    strings.Builder
	stderr    strings.Builder
	pending   string
	removed   int64
	parseErr  error
	truncated bool
}

func (o *imagePruneOutput) write(chunk runtimes.OutputChunk) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch chunk.Stream {
	case "stdout":
		o.append(&o.stdout, string(chunk.Data))
		lines := strings.Split(o.pending+string(chunk.Data), "\n")
		for _, line := range lines[:len(lines)-1] {
			o.parseLine(line)
		}
		o.pending = lines[len(lines)-1]
		if len(o.pending) > maxCleanupOutput {
			return errors.New("image prune output line exceeds the result limit")
		}
	case "stderr":
		o.append(&o.stderr, string(chunk.Data))
	default:
		return fmt.Errorf("unknown image prune output stream %q", chunk.Stream)
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

func (o *imagePruneOutput) result(runErr error) (app.DockerCleanupStep, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.parseLine(o.pending)
	output := strings.TrimSpace(o.stdout.String())
	diagnostics := strings.TrimSpace(o.stderr.String())
	var diagnosticErr error
	if diagnostics != "" {
		// nerdctl logs some image deletion failures as warnings but exits zero.
		diagnosticErr = fmt.Errorf("image cleanup reported runtime diagnostics: %s", diagnostics)
		output = strings.TrimSpace(output + "\n" + diagnostics)
	}
	if o.truncated {
		output += "\n[Runtime output truncated; reported removal counts include all parsed lines.]"
	}
	err := errors.Join(runErr, o.parseErr, diagnosticErr)
	result := app.DockerCleanupStep{Status: app.CleanupSucceeded, ItemsRemoved: o.removed, Output: output}
	if err != nil {
		result.Status = app.CleanupFailed
		result.Error = err.Error()
	}
	return result, err
}
