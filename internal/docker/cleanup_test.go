package docker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/runtimes"
	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/grpc"
)

type cleanupBuildKitServer struct {
	buildKitPlatformServer
	pruneMu    sync.Mutex
	pruneCalls int
	all        bool
	records    []*controlapi.UsageRecord
	pruneErr   error
	wait       bool
}

func (s *cleanupBuildKitServer) Prune(request *controlapi.PruneRequest, stream grpc.ServerStreamingServer[controlapi.UsageRecord]) error {
	s.pruneMu.Lock()
	s.pruneCalls++
	s.all = request.All
	s.pruneMu.Unlock()
	if len(request.Filter) != 0 || request.KeepDuration != 0 {
		return errors.New("cleanup changed the requested all-unused cache semantics")
	}
	for _, record := range s.records {
		if err := stream.Send(record); err != nil {
			return err
		}
	}
	if s.wait {
		<-stream.Context().Done()
		return context.Cause(stream.Context())
	}
	return s.pruneErr
}

func TestCleanupUnusedRunsOneCachePassAndReportsImageReferences(t *testing.T) {
	service := &cleanupBuildKitServer{records: []*controlapi.UsageRecord{
		{ID: "synthetic-cache-1", Size: 1024},
		{ID: "synthetic-cache-2", Size: 2048},
	}}
	imageCalls := 0
	runner := &fakeRunner{streamer: func(command runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		imageCalls++
		if command.Name != "nerdctl" || !reflect.DeepEqual(command.Args, []string{"image", "prune", "--all", "--force", "--filter", "until=24h0m0s"}) {
			return nil, errors.New("cleanup must invoke only native unused-image pruning")
		}
		for _, data := range []string{
			"Deleted Images:\nUntag",
			"ged: example/unused:one\ndeleted: sha256:synthetic-layer\nUntagged: example/unused:two\n\n",
		} {
			if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte(data)}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}}
	manager := New(runner)
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	result, err := manager.CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if imageCalls != 1 || service.pruneCalls != 1 || !service.all {
		t.Fatalf("duplicate or incomplete prune: image calls=%d, cache calls=%d, all=%t", imageCalls, service.pruneCalls, service.all)
	}
	if result.BuildCache.Status != app.CleanupSucceeded || result.BuildCache.ItemsRemoved != 2 ||
		result.BuildCache.BytesReclaimed == nil || *result.BuildCache.BytesReclaimed != 3072 {
		t.Fatalf("incorrect build-cache result: %+v", result.BuildCache)
	}
	if result.Images.Status != app.CleanupSucceeded || result.Images.ItemsRemoved != 2 ||
		result.Images.BytesReclaimed != nil || !strings.Contains(result.Images.Output, "example/unused:two") {
		t.Fatalf("incorrect image result: %+v", result.Images)
	}
	if result.Containers.Status != app.CleanupSucceeded || result.Volumes.Status != app.CleanupSucceeded ||
		result.HostDisk.Status != app.CleanupSkipped {
		t.Fatalf("cleanup skipped a runtime step or claimed a host disk it does not own: %+v", result)
	}
}

func TestCleanupUnusedRetainsPlatformWarningsWithoutFailing(t *testing.T) {
	warning := `time="2026-01-01T00:00:00Z" level=warning msg="failed to enumerate rootfs" error="no match for platform in manifest sha256:` +
		strings.Repeat("a", 64) + `: not found"`
	manager := New(&fakeRunner{streamer: func(_ runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		for _, chunk := range []runtimes.OutputChunk{
			{Stream: "stderr", Data: []byte(warning[:70])},
			{Stream: "stdout", Data: []byte("Deleted Images:\nUntagged: example/foreign-platform:unused\n")},
			{Stream: "stderr", Data: []byte(warning[70:])},
		} {
			if err := emit(chunk); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}})
	manager.dialBuildKit = buildKitControlTestDialer(t, &cleanupBuildKitServer{})

	result, err := manager.CleanupUnused(context.Background())
	if err != nil || result.Images.Status != app.CleanupSucceeded || result.Images.Error != "" {
		t.Fatalf("a layer-reporting warning failed successful image deletion: result=%+v, error=%v", result, err)
	}
	if result.Images.ItemsRemoved != 1 || result.Images.BytesReclaimed != nil ||
		!strings.Contains(result.Images.Output, warning) {
		t.Fatalf("cleanup lost its deletion count or advisory warning: %+v", result.Images)
	}
}

func TestCleanupUnusedPreservesActiveBuilds(t *testing.T) {
	service := &cleanupBuildKitServer{}
	service.active = true
	imageCalls := 0
	manager := New(&fakeRunner{streamer: func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error) {
		imageCalls++
		return nil, nil
	}})
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	result, err := manager.CleanupUnused(context.Background())
	if !errors.Is(err, ErrConflict) || service.pruneCalls != 0 || imageCalls != 0 ||
		result.BuildCache.Status != app.CleanupNotRun || result.Images.Status != app.CleanupNotRun {
		t.Fatalf("active build was not protected: result=%+v, error=%v, cache=%d, images=%d", result, err, service.pruneCalls, imageCalls)
	}
}

func TestCleanupUnusedRetainsPartialResultsAndSoftFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		cacheError error
		imageError error
		stderr     string
		stdout     string
	}{
		{name: "cache failure", cacheError: errors.New("synthetic cache failure")},
		{name: "image exit failure", imageError: errors.New("synthetic image failure")},
		{name: "image warning with zero exit", stderr: "time=\"2026-01-01T00:00:00Z\" level=warning msg=\"failed to delete image example/unused:two\"\n"},
		{name: "other rootfs warning", stderr: `level=warning msg="failed to enumerate rootfs" error="synthetic content read failure"`},
		{name: "rootfs error severity", stderr: `level=error msg="failed to enumerate rootfs" error="no match for platform in manifest sha256:` + strings.Repeat("a", 64) + `: not found"`},
		{name: "unknown image output", stdout: "unexpected prune output\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &cleanupBuildKitServer{
				records:  []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: 4096}},
				pruneErr: test.cacheError,
			}
			manager := New(&fakeRunner{streamer: func(_ runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
				if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Deleted Images:\nUntagged: example/unused:one\n" + test.stdout)}); err != nil {
					return nil, err
				}
				if test.stderr != "" {
					if err := emit(runtimes.OutputChunk{Stream: "stderr", Data: []byte(test.stderr)}); err != nil {
						return nil, err
					}
				}
				return nil, test.imageError
			}})
			manager.dialBuildKit = buildKitControlTestDialer(t, service)
			result, err := manager.CleanupUnused(context.Background())
			if err == nil {
				t.Fatal("cleanup hid a failure or unsupported output")
			}
			if result.BuildCache.ItemsRemoved != 1 || result.Images.ItemsRemoved != 1 {
				t.Fatalf("cleanup lost partial deletion counts: %+v", result)
			}
			if test.cacheError != nil {
				if result.BuildCache.Status != app.CleanupFailed || result.BuildCache.Error == "" ||
					result.Images.Status != app.CleanupSucceeded {
					t.Fatalf("independent image cleanup did not finish after a cache failure: %+v", result)
				}
			} else if result.Images.Status != app.CleanupFailed || result.Images.Error == "" {
				t.Fatalf("image failure was not retained: %+v", result)
			}
		})
	}
}

func TestImagePruneDiagnosticsRemainCheckedAfterOutputTruncation(t *testing.T) {
	warning := `level=warning msg="failed to enumerate rootfs" error="no match for platform in manifest sha256:` +
		strings.Repeat("b", 64) + `: not found"` + "\n"
	for _, test := range []struct {
		name    string
		failure string
		runErr  error
	}{
		{name: "advisory warnings only"},
		{name: "deletion failure", failure: `level=warning msg="failed to delete image example/unused:two"`},
		{name: "command failure", runErr: errors.New("synthetic prune exit failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output imagePruneOutput
			diagnostics := strings.Repeat(warning, maxCleanupOutput/len(warning)+2) + test.failure
			if err := output.write(runtimes.OutputChunk{Stream: "stderr", Data: []byte(diagnostics)}); err != nil {
				t.Fatal(err)
			}
			result, err := output.result(test.runErr)
			if !strings.Contains(result.Output, "truncated") || len(result.Output) > maxCleanupOutput+128 {
				t.Fatalf("diagnostic output was not bounded: %+v", result)
			}
			if test.failure == "" && test.runErr == nil {
				if err != nil || result.Status != app.CleanupSucceeded {
					t.Fatalf("truncating advisory output failed cleanup: %+v, %v", result, err)
				}
			} else if err == nil || result.Status != app.CleanupFailed ||
				(test.failure != "" && !strings.Contains(result.Error, test.failure)) ||
				(test.runErr != nil && !errors.Is(err, test.runErr)) {
				t.Fatalf("truncation hid an actual deletion failure: %+v, %v", result, err)
			}
		})
	}
}

func TestCleanupUnusedCancellationDoesNotStartImagePrune(t *testing.T) {
	service := &cleanupBuildKitServer{wait: true}
	imageCalls := 0
	manager := New(&fakeRunner{streamer: func(runtimes.Command, func(runtimes.OutputChunk) error) ([]byte, error) {
		imageCalls++
		return nil, nil
	}})
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result, err := manager.CleanupUnused(ctx)
	if err == nil || imageCalls != 0 || result.Images.Status != app.CleanupNotRun {
		t.Fatalf("canceled cleanup started more deletions: result=%+v, error=%v, image calls=%d", result, err, imageCalls)
	}
}

func TestCleanupUnusedIsScopedToOwnedLimaEngine(t *testing.T) {
	directory := filepath.Join(t.TempDir(), engineInstanceName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "disk"), []byte("synthetic disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := json.Marshal(map[string]string{"name": engineInstanceName, "status": "Running", "dir": directory})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: map[string][]byte{
		"limactl list porto-engine --json": listing,
		`limactl shell --workdir=/ porto-engine -- sh -c cat "$HOME/.porto-engine-owner"`: []byte("synthetic-owner\n"),
		"limactl shell --workdir=/ porto-engine -- sudo -n fstrim --all --verbose":         []byte("/: 1 MiB (1048576 bytes) trimmed on /dev/vda1\n"),
	}}
	runner.streamer = func(command runtimes.Command, _ func(runtimes.OutputChunk) error) ([]byte, error) {
		if command.Name != "limactl" || strings.Join(command.Args, " ") != "shell --workdir=/ porto-engine -- nerdctl image prune --all --force --filter until=24h0m0s" {
			return nil, errors.New("cleanup escaped the owned backend or used the host working directory")
		}
		return nil, nil
	}
	manager := NewWithStateDir(runner, t.TempDir())
	manager.lookPath = func(name string) (string, error) { return name, nil }
	manager.dialBuildKit = buildKitControlTestDialer(t, &cleanupBuildKitServer{})
	if err := manager.writeEngineState(engineState{Mode: "lima", Instance: engineInstanceName, OwnerID: "synthetic-owner"}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.HostDisk.Status != app.CleanupSucceeded || result.HostDisk.BytesReclaimed == nil ||
		!strings.Contains(result.HostDisk.Output, "1048576 bytes") {
		t.Fatalf("owned engine disk was not trimmed and measured: %+v", result.HostDisk)
	}
}

func TestCleanupUnusedReportsEngineDisksWithoutDiscardAsSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(path, []byte("synthetic disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := New(&fakeRunner{errors: map[string]error{
		"limactl shell --workdir=/ porto-engine -- sudo -n fstrim --all --verbose": errors.New("fstrim: /: the discard operation is not supported"),
	}})
	step, err := manager.reclaimHostDisk(context.Background(), hostDiskTarget{instance: engineInstanceName, path: path})
	if err != nil || step.Status != app.CleanupSkipped || step.Error != "" || !strings.Contains(step.Output, "does not support discard") {
		t.Fatalf("unsupported discard was not reported as skipped: %+v, %v", step, err)
	}
}

func TestCleanupUnusedDoesNotInventUnknownCacheSizes(t *testing.T) {
	service := &cleanupBuildKitServer{records: []*controlapi.UsageRecord{{ID: "synthetic-cache", Size: -1}}}
	manager := New(&fakeRunner{})
	manager.dialBuildKit = buildKitControlTestDialer(t, service)
	result, err := manager.CleanupUnused(context.Background())
	if err != nil || result.BuildCache.ItemsRemoved != 1 || result.BuildCache.BytesReclaimed != nil {
		t.Fatalf("unknown cache size was reported as a known byte count: %+v, %v", result, err)
	}
}

func TestCleanupUnusedBoundsOutputWithoutLosingRemovalCounts(t *testing.T) {
	const references = 2000
	manager := New(&fakeRunner{streamer: func(_ runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
		for range references {
			if err := emit(runtimes.OutputChunk{Stream: "stdout", Data: []byte("Untagged: example/synthetic-image:unused\n")}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}})
	manager.dialBuildKit = buildKitControlTestDialer(t, &cleanupBuildKitServer{})
	result, err := manager.CleanupUnused(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Images.ItemsRemoved != references || len(result.Images.Output) > maxCleanupOutput+128 ||
		!strings.Contains(result.Images.Output, "truncated") {
		t.Fatalf("unbounded or inaccurate image prune output: count=%d, bytes=%d", result.Images.ItemsRemoved, len(result.Images.Output))
	}
}

func TestCleanupUnusedRejectsOverlapBeforeAnyMutation(t *testing.T) {
	manager := New(&fakeRunner{})
	manager.cleanupMu.Lock()
	defer manager.cleanupMu.Unlock()
	if _, err := manager.CleanupUnused(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping cleanup = %v, want conflict", err)
	}
}

func TestCleanupUnusedRequiresDiagnosticCaptureBeforePruning(t *testing.T) {
	manager := New(&cancellationCleanupRunner{})
	if _, err := manager.CleanupUnused(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("non-streaming cleanup = %v, want unsupported", err)
	}
}
