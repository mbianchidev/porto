package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mbianchidev/porto/internal/dataops"
)

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
