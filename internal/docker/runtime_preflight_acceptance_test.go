package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mbianchidev/porto/internal/dataops"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func TestRuntimeDataReadTraceLocatesTimeoutWithoutExtendingItsDeadline(t *testing.T) {
	for _, guestStarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest-started-%t", guestStarted), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				command := runtimes.Command{
					Name: "limactl", Dir: "synthetic directory", Env: []string{"SYNTHETIC_MODE=trace"},
					Args: []string{"shell", "synthetic-engine", "--", "sh", "-c", limaOwnedRuntimeCommand,
						"porto-runtime-command", "synthetic-owner", "nerdctl", "volume", "ls", "--format", "{{json .}}"},
				}
				original := slices.Clone(command.Args)
				started := time.Now()
				runner := contextImageRunner{stream: func(actual context.Context, traced runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
					if actual != ctx || traced.Dir != command.Dir || !slices.Equal(traced.Env, command.Env) {
						t.Fatal("read trace changed the original context or environment")
					}
					if !slices.Equal(traced.Args[:3], original[:3]) ||
						!slices.Equal(traced.Args[len(traced.Args)-len(original)+3:], original[3:]) {
						t.Fatal("read trace changed the instance, ownership guard or guest arguments")
					}
					if guestStarted {
						time.Sleep(4 * time.Second)
						for _, part := range []string{"PORTO_CI_META", "DATA_STARTED\n"} {
							if err := emit(runtimes.OutputChunk{Stream: "stderr", Data: []byte(part)}); err != nil {
								return nil, err
							}
						}
					}
					<-actual.Done()
					return nil, actual.Err()
				}}
				output, err := runtimeDataTraceRead(ctx, command, runtimeDataReadGuestOffset(command), runner)
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 20*time.Second {
					t.Fatalf("read trace changed the original deadline: elapsed=%s err=%v", time.Since(started), err)
				}
				at := time.Duration(0)
				if guestStarted {
					at = 4 * time.Second
				}
				want := fmt.Sprintf("guest start observed=%t at %s, guest finish observed=false", guestStarted, at)
				if !strings.Contains(string(output), want) || strings.Contains(string(output), "PORTO_CI_METADATA_") ||
					!slices.Equal(command.Args, original) {
					t.Fatalf("read timing was inaccurate or changed the original command: %s", output)
				}
			})
		})
	}
}

func TestRuntimeDataReadTracePreservesGuestExitStatusAndJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the timing wrapper executes in the Linux guest")
	}
	for _, status := range []string{"0", "7"} {
		t.Run("status-"+status, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\nprintf '{\"Name\":\"synthetic-volume\"}\\n'\nif [ \"$SYNTHETIC_STATUS\" != 0 ]; then printf 'synthetic guest error\\n' >&2; fi\nexit \"$SYNTHETIC_STATUS\"\n"
			if err := os.WriteFile(filepath.Join(bin, "nerdctl"), []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			command := runtimes.Command{
				Name: "limactl", Args: []string{"shell", "synthetic-engine", "--", "nerdctl", "volume", "ls"},
				Env: []string{"SYNTHETIC_STATUS=" + status, "PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")},
			}
			runner := contextImageRunner{stream: func(ctx context.Context, traced runtimes.Command, emit func(runtimes.OutputChunk) error) ([]byte, error) {
				traced.Name, traced.Args = traced.Args[3], traced.Args[4:]
				return (runtimes.ExecRunner{}).RunStreaming(ctx, traced, emit)
			}}
			output, err := runtimeDataTraceRead(context.Background(), command, runtimeDataReadGuestOffset(command), runner)
			if status == "0" {
				if err != nil || string(output) != "{\"Name\":\"synthetic-volume\"}\n" {
					t.Fatalf("timing markers polluted successful metadata: %q, %v", output, err)
				}
				return
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(string(output), "synthetic guest error") ||
				!strings.Contains(string(output), "guest start observed=true") || !strings.Contains(string(output), "guest finish observed=true") {
				t.Fatalf("read trace hid the guest error or exit status: %s, %v", output, err)
			}
		})
	}
}

func TestRuntimeDataReadTimeoutDiagnosticsPreserveFailureAndOwnership(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	command := runtimes.Command{
		Name: "limactl", Dir: "synthetic directory", Env: []string{"SYNTHETIC_MODE=diagnostic"},
		Args: []string{"shell", "synthetic-engine", "--", "sh", "-c", limaOwnedRuntimeCommand,
			"porto-runtime-command", "synthetic-owner", "nerdctl", "volume", "ls", "--format", "{{json .}}"},
	}
	original := slices.Clone(command.Args)
	failure := errors.New("synthetic original timeout")
	calls := 0
	output, err := runtimeDataReadTimeoutDiagnostics(ctx, command, []byte("original output"), failure, lifecycleDeadlineRunner(func(ctx context.Context, probe runtimes.Command) ([]byte, error) {
		calls++
		if ctx.Err() != nil {
			t.Fatal("diagnostic inherited the expired command context")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("diagnostic lost its bounded shared deadline")
		}
		if probe.Dir != command.Dir || !slices.Equal(probe.Env, command.Env) {
			t.Fatal("diagnostic changed the fixture's command environment")
		}
		prefix := []string{"--debug", "shell", "--workdir=/", "synthetic-engine", "--"}
		if !slices.Equal(probe.Args[:len(prefix)], prefix) {
			t.Fatalf("diagnostic changed the selected instance: %v", probe.Args)
		}
		if calls == 1 && !slices.Equal(probe.Args[len(prefix):], []string{"true"}) {
			t.Fatal("SSH startup probe performed a guest operation")
		}
		if calls == 2 && !slices.Equal(probe.Args[len(probe.Args)-(len(original)-3):], original[3:]) {
			t.Fatal("read timing probe discarded the exact original ownership check or arguments")
		}
		return []byte("synthetic diagnostic output"), errors.New("synthetic diagnostic failure")
	}))
	if err != failure || calls != 2 || !slices.Equal(command.Args, original) {
		t.Fatalf("diagnostics altered the original failure or request: err=%v calls=%d", err, calls)
	}
	if !strings.HasPrefix(string(output), "original output") ||
		!strings.Contains(string(output), "synthetic diagnostic failure") {
		t.Fatalf("original or diagnostic failures were hidden: %s", output)
	}
}

func TestRuntimeDataReadTimeoutDiagnosticsNeverReplayMutationsOrArbitraryShells(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	failure := errors.New("synthetic failure")
	for _, command := range []runtimes.Command{
		{Name: "nerdctl", Args: []string{"volume", "ls"}},
		{Name: "limactl", Args: []string{"shell", "synthetic", "--", "nerdctl", "volume", "rm", "synthetic"}},
		{Name: "limactl", Args: []string{"shell", "synthetic", "--", "sh", "-c", "synthetic mutation", "nerdctl", "volume", "ls"}},
		{Name: "limactl", Args: []string{"shell", "synthetic"}},
	} {
		if runtimeDataReadGuestOffset(command) >= 0 {
			t.Fatal("read tracing accepted a non-allowlisted operation")
		}
		output, err := runtimeDataReadTimeoutDiagnostics(ctx, command, []byte("original"), failure, lifecycleDeadlineRunner(func(context.Context, runtimes.Command) ([]byte, error) {
			t.Fatal("diagnostic replayed a non-allowlisted operation")
			return nil, nil
		}))
		if err != failure || string(output) != "original" {
			t.Fatal("non-allowlisted failure changed")
		}
	}
	command := runtimes.Command{Name: "limactl", Args: []string{"shell", "synthetic", "--", "nerdctl", "volume", "ls"}}
	for _, original := range []struct {
		ctx context.Context
		err error
	}{{context.Background(), failure}, {ctx, nil}} {
		_, err := runtimeDataReadTimeoutDiagnostics(original.ctx, command, nil, original.err, lifecycleDeadlineRunner(func(context.Context, runtimes.Command) ([]byte, error) {
			t.Fatal("diagnostic replayed a successful or non-timeout request")
			return nil, nil
		}))
		if err != original.err {
			t.Fatal("original outcome changed")
		}
	}
}

func (fixture *runtimeDataFixture) cleanupSelections() []dataops.Selection {
	pending := append(slices.Clone(fixture.selection), fixture.extraSelections...)
	for _, tag := range []string{fixture.prefix + ":fixture", fixture.prefix + ":alias"} {
		pending = append(pending, dataops.Selection{Kind: "image", Destination: tag})
	}
	var ordered []dataops.Selection
	for _, kind := range []string{"container", "volume", "network", "image"} {
		seen := make(map[string]bool)
		for _, selection := range pending {
			if selection.Kind != kind || seen[selection.Destination] {
				continue
			}
			seen[selection.Destination] = true
			ordered = append(ordered, selection)
		}
	}
	return ordered
}

func cleanupRuntimeDataDestinations(ctx context.Context, selections []dataops.Selection, remove func(context.Context, dataops.Selection) error) error {
	failures := make([]error, len(selections))
	removeOne := func(index int, selection dataops.Selection) {
		err := remove(ctx, selection)
		if err != nil && !errorsIsNotFound(err) && !missingDockerObject(err, selection.Kind) {
			failures[index] = fmt.Errorf("remove exact acceptance destination %s: %w", selection.Destination, err)
		}
	}
	var pending sync.WaitGroup
	slots := make(chan struct{}, 2)
	for index, selection := range selections {
		if selection.Kind != "volume" && selection.Kind != "network" {
			pending.Wait()
			removeOne(index, selection)
			continue
		}
		slots <- struct{}{}
		pending.Add(1)
		go func() {
			defer pending.Done()
			defer func() { <-slots }()
			removeOne(index, selection)
		}()
	}
	pending.Wait()
	return errors.Join(failures...)
}

func TestRuntimeDataCleanupBoundsIndependentRemovals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := runtimeDataFixture{
			prefix: "synthetic",
			selection: []dataops.Selection{
				{Kind: "container", Destination: "app"},
				{Kind: "volume", Destination: "volume-a"},
				{Kind: "network", Destination: "network"},
			},
			extraSelections: []dataops.Selection{
				{Kind: "volume", Destination: "volume-b"},
				{Kind: "volume", Destination: "volume-c"},
			},
		}
		selections := fixture.cleanupSelections()
		var mu sync.Mutex
		active, peak, completedIndependent := 0, 0, 0
		containerDone := false
		calls := make(map[string]int)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		started := time.Now()
		err := cleanupRuntimeDataDestinations(ctx, selections, func(ctx context.Context, selection dataops.Selection) error {
			mu.Lock()
			independent := selection.Kind == "volume" || selection.Kind == "network"
			switch {
			case selection.Kind == "container" && active != 0:
				t.Error("container cleanup overlapped another resource")
			case independent && !containerDone:
				t.Error("dependent resources were removed before the container")
			case selection.Kind == "image" && (active != 0 || completedIndependent != 4):
				t.Error("image aliases were removed concurrently or before their dependents")
			}
			active++
			peak = max(peak, active)
			calls[selection.Kind+"/"+selection.Destination]++
			mu.Unlock()
			duration := 16 * time.Second
			switch selection.Kind {
			case "container":
				duration = 32 * time.Second
			case "volume":
				duration = 24 * time.Second
			}
			var err error
			select {
			case <-time.After(duration):
			case <-ctx.Done():
				err = ctx.Err()
			}
			mu.Lock()
			active--
			if selection.Kind == "container" {
				containerDone = err == nil
			}
			if independent && err == nil {
				completedIndependent++
			}
			mu.Unlock()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed != 112*time.Second || peak != 2 {
			t.Fatalf("cleanup took %s with %d concurrent removals, want 112s with at most two", elapsed, peak)
		}
		for _, selection := range selections {
			if count := calls[selection.Kind+"/"+selection.Destination]; count != 1 {
				t.Fatalf("destination %s removed %d times", selection.Destination, count)
			}
		}
	})
}

func TestRuntimeDataCleanupPreservesEveryFailure(t *testing.T) {
	foreign := errors.New("synthetic foreign ownership")
	failed := errors.New("synthetic removal failure")
	selections := []dataops.Selection{
		{Kind: "container", Destination: "foreign"},
		{Kind: "volume", Destination: "missing"},
		{Kind: "network", Destination: "failed"},
		{Kind: "image", Destination: "remaining"},
	}
	var mu sync.Mutex
	calls := make(map[string]int)
	err := cleanupRuntimeDataDestinations(context.Background(), selections, func(_ context.Context, selection dataops.Selection) error {
		mu.Lock()
		calls[selection.Destination]++
		mu.Unlock()
		switch selection.Destination {
		case "foreign":
			return foreign
		case "missing":
			return ErrNotFound
		case "failed":
			return failed
		default:
			return nil
		}
	})
	if !errors.Is(err, foreign) || !errors.Is(err, failed) || errors.Is(err, ErrNotFound) {
		t.Fatalf("cleanup lost a real failure or rejected an already absent object: %v", err)
	}
	for _, selection := range selections {
		if calls[selection.Destination] != 1 {
			t.Fatalf("cleanup skipped or repeated %s after an earlier failure", selection.Destination)
		}
	}
}

func TestRuntimeDataCleanupDestinationsOnceInDependencyOrder(t *testing.T) {
	fixture := runtimeDataFixture{
		prefix: "synthetic",
		selection: []dataops.Selection{
			{Kind: "image", Destination: "synthetic:alias"},
			{Kind: "volume", Destination: "shared-name", ID: "original-volume"},
			{Kind: "network", Destination: "shared-name"},
			{Kind: "container", Destination: "shared-name"},
		},
		extraSelections: []dataops.Selection{
			{Kind: "volume", Destination: "shared-name", ID: "repeated-volume"},
			{Kind: "volume", Destination: "extra-volume"},
			{Kind: "image", Destination: "synthetic:alias"},
			{Kind: "image", Destination: "synthetic:fixture"},
		},
	}
	original := slices.Clone(fixture.selection)
	extra := slices.Clone(fixture.extraSelections)
	got := fixture.cleanupSelections()
	var destinations []string
	for _, selection := range got {
		destinations = append(destinations, selection.Kind+"/"+selection.Destination)
	}
	want := []string{
		"container/shared-name", "volume/shared-name", "volume/extra-volume",
		"network/shared-name", "image/synthetic:alias", "image/synthetic:fixture",
	}
	if !slices.Equal(destinations, want) {
		t.Fatalf("cleanup destinations = %q, want %q", destinations, want)
	}
	if got[1].ID != "original-volume" || !slices.Equal(fixture.selection, original) || !slices.Equal(fixture.extraSelections, extra) {
		t.Fatal("cleanup changed the fixture's recorded selections")
	}
}

func (fixture *runtimeDataFixture) removeDestination(ctx context.Context, selection dataops.Selection) error {
	ctx, release, err := fixture.manager.dataReadGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	document, err := fixture.manager.inspect(ctx, selection.Kind, selection.Destination)
	if missingDockerObject(err, selection.Kind) {
		return nil
	}
	if err != nil {
		return err
	}
	var resource struct {
		ID     string `json:"Id"`
		Labels map[string]string
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(document, &resource); err != nil {
		return err
	}
	labels := resource.Labels
	if selection.Kind == "container" || selection.Kind == "image" {
		labels = resource.Config.Labels
	}
	if labels["porto.test.suite"] != fixture.prefix {
		return errors.New("acceptance destination ownership changed; nothing was removed")
	}
	switch selection.Kind {
	case "container":
		return fixture.manager.ContainerAction(ctx, resource.ID, "remove-force")
	case "volume":
		return fixture.manager.removeVolume(ctx, selection.Destination, false, document)
	case "network":
		return fixture.manager.RemoveNetwork(ctx, selection.Destination)
	case "image":
		return fixture.manager.RemoveImage(ctx, selection.Destination, false)
	default:
		return fmt.Errorf("unknown fixture destination kind %s", selection.Kind)
	}
}

func (fixture *runtimeDataFixture) migrationPreflight(t *testing.T) {
	volume := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "volume" })]
	image := fixture.selection[slices.IndexFunc(fixture.selection, func(selection dataops.Selection) bool { return selection.Kind == "image" })]
	t.Run("foreign destination conflicts never overwrite data", func(t *testing.T) {
		name := fixture.prefix + "-foreign-destination"
		if _, err := fixture.manager.CreateVolume(fixture.ctx, name, "local", map[string]string{"porto.test.suite": fixture.prefix}); err != nil {
			t.Fatal(err)
		}
		selection := volume
		selection.Destination = name
		fixture.extraSelections = append(fixture.extraSelections, selection)
		descriptor, err := fixture.manager.FileDescriptor(fixture.ctx, "volume", name)
		if err != nil {
			t.Fatal(err)
		}
		preview, err := fixture.manager.PreviewMigration(fixture.ctx, dataops.Request{Context: fixture.source.context.Name, Selections: []dataops.Selection{selection}})
		if err != nil || len(preview.Conflicts) == 0 {
			t.Fatalf("existing unverified volume was not a deterministic conflict: %+v, %v", preview.Conflicts, err)
		}
		preview.Request.Confirm = true
		result, err := fixture.manager.Migrate(fixture.ctx, preview.Request, fixture.root, func(string, int64) error { t.Error("conflicting migration attempted a mutation"); return nil }, func(dataops.Result) error { return nil })
		if err == nil || len(result.Steps) != 0 {
			t.Fatalf("conflicting destination acquired a successful transfer: %+v, %v", result, err)
		}
		current, err := fixture.manager.FileDescriptor(fixture.ctx, "volume", name)
		if err != nil || current.Resource.Fingerprint() != descriptor.Resource.Fingerprint() {
			t.Fatalf("naming conflict replaced original destination data: %v", err)
		}
	})
	t.Run("unsupported source settings are reported before creation", func(t *testing.T) {
		name := fixture.prefix + "-unsupported-container"
		var created struct {
			ID string `json:"Id"`
		}
		fixture.sourceJSON(http.MethodPost, "/containers/create", url.Values{"name": {name}}, map[string]any{
			"Image": fixture.prefix + ":fixture", "Cmd": []string{"sh", "-c", "sleep 600"},
			"Labels":     map[string]string{"porto.test.suite": fixture.prefix},
			"HostConfig": map[string]any{"NetworkMode": "none", "CpuShares": 512},
		}, &created)
		destination := fixture.prefix + "-unsupported-destination"
		request := dataops.Request{Context: fixture.source.context.Name, Selections: []dataops.Selection{image, {Kind: "container", Name: name, ID: created.ID, Destination: destination}}}
		preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
		if err != nil || !strings.Contains(strings.Join(preview.Conflicts, "\n"), "CPU scheduling") {
			t.Fatalf("unsupported source options were not explained: %+v, %v", preview.Conflicts, err)
		}
		if _, err := fixture.manager.InspectContainer(fixture.ctx, destination); !missingDockerObject(err, "container") {
			t.Fatalf("unsupported source settings created a destination: %v", err)
		}
	})
	t.Run("sensitive environment requires separate current consent", func(t *testing.T) {
		name := fixture.prefix + "-sensitive"
		var created struct {
			ID string `json:"Id"`
		}
		fixture.sourceJSON(http.MethodPost, "/containers/create", url.Values{"name": {name}}, map[string]any{
			"Image": fixture.prefix + ":fixture", "Env": []string{"SYNTHETIC_TOKEN=mock-transfer-value"},
			"Labels":     map[string]string{"porto.test.suite": fixture.prefix},
			"HostConfig": map[string]any{"NetworkMode": "none"},
		}, &created)
		request := dataops.Request{Context: fixture.source.context.Name, Selections: []dataops.Selection{image, {Kind: "container", Name: name, ID: created.ID, Destination: fixture.prefix + "-sensitive-destination"}}}
		preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
		if err != nil || !strings.Contains(strings.Join(preview.Conflicts, "\n"), "explicit local-transfer consent") {
			t.Fatalf("sensitive environment was selected implicitly: %+v, %v", preview.Conflicts, err)
		}
		request.IncludeSensitive = true
		preview, err = fixture.manager.PreviewMigration(fixture.ctx, request)
		if err != nil || len(preview.Conflicts) != 0 {
			t.Fatalf("explicit supported local consent was lost: %+v, %v", preview.Conflicts, err)
		}
	})
	t.Run("unrepresentable rootless internal published ports fail before creation", func(t *testing.T) {
		backend, err := fixture.manager.backend(fixture.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if backend.limaInstance == "" {
			t.Skip("this cell uses the rootful local kernel, not rootless Lima publishing")
		}
		networkName := fixture.prefix + "-internal"
		var network, container struct {
			ID string `json:"Id"`
		}
		fixture.sourceJSON(http.MethodPost, "/networks/create", nil, map[string]any{
			"Name": networkName, "Driver": "bridge", "Internal": true, "Labels": map[string]string{"porto.test.suite": fixture.prefix},
		}, &network)
		name := fixture.prefix + "-internal-app"
		fixture.sourceJSON(http.MethodPost, "/containers/create", url.Values{"name": {name}}, map[string]any{
			"Image": fixture.prefix + ":fixture", "Labels": map[string]string{"porto.test.suite": fixture.prefix},
			"HostConfig": map[string]any{"NetworkMode": networkName, "PortBindings": map[string]any{"8080/tcp": []map[string]string{{"HostIP": "127.0.0.1", "HostPort": "12345"}}}},
		}, &container)
		request := dataops.Request{Context: fixture.source.context.Name, Selections: []dataops.Selection{image,
			{Kind: "network", Name: networkName, ID: network.ID, Destination: fixture.prefix + "-internal-destination"},
			{Kind: "container", Name: name, ID: container.ID, Destination: fixture.prefix + "-internal-app-destination"},
		}}
		preview, err := fixture.manager.PreviewMigration(fixture.ctx, request)
		if err != nil || !strings.Contains(strings.Join(preview.Conflicts, "\n"), "rootless published ports on internal networks") {
			t.Fatalf("unsupported network/publishing semantics were not reported: %+v, %v", preview.Conflicts, err)
		}
	})
}
