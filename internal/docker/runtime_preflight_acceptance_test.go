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
	"testing"

	"github.com/mbianchidev/porto/internal/dataops"
)

func (fixture *runtimeDataFixture) removeDestination(ctx context.Context, selection dataops.Selection) error {
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
		return fixture.manager.RemoveVolume(ctx, selection.Destination, false)
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
