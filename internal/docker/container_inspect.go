package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func containerNetworkNames(container Container) []string {
	names := map[string]string{}
	for _, endpoint := range container.NetworkDetails {
		if endpoint.Name != "" {
			names[endpoint.Name] = ""
		}
	}
	for _, name := range strings.Split(container.Networks, ",") {
		if name = strings.TrimSpace(name); name != "" {
			names[name] = ""
		}
	}
	return sortedStringKeys(names)
}

func (m *Manager) containerNetworkIdentities(ctx context.Context, names []string) (map[string]string, error) {
	selected := map[string]string{}
	for _, name := range names {
		if name == "host" || name == "none" || name == "" || strings.HasPrefix(name, "container:") || strings.HasPrefix(name, "ns:") {
			continue
		}
		if err := validateObjectID(name); err != nil {
			return nil, fmt.Errorf("invalid container network identity: %w", err)
		}
		selected[name] = ""
	}
	if len(selected) == 0 || m.IsNativeWindows() {
		return selected, nil
	}
	args := append([]string{"network", "inspect"}, sortedStringKeys(selected)...)
	output, err := m.runStdout(ctx, "inspect Porto container network identities", args...)
	if err != nil {
		return nil, err
	}
	var networks []struct{ Name, ID string }
	if err := json.Unmarshal(output, &networks); err != nil {
		return nil, fmt.Errorf("decode canonical container network identities: %w", err)
	}
	seen := map[string]bool{}
	for _, network := range networks {
		if _, ok := selected[network.Name]; ok {
			if seen[network.Name] && selected[network.Name] != network.ID {
				return nil, fmt.Errorf("%w: attached network %q has conflicting identities", ErrConflict, network.Name)
			}
			selected[network.Name] = network.ID
			seen[network.Name] = true
		}
	}
	for name := range selected {
		if !seen[name] {
			return nil, fmt.Errorf("%w: attached network %q could not be identified", ErrUnavailable, name)
		}
	}
	return selected, nil
}

func (m *Manager) completeInspectNetworkIdentities(ctx context.Context, document json.RawMessage) (json.RawMessage, error) {
	var inspected map[string]json.RawMessage
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, err
	}
	var settings map[string]json.RawMessage
	if len(inspected["NetworkSettings"]) == 0 || string(inspected["NetworkSettings"]) == "null" {
		return document, nil
	}
	if err := json.Unmarshal(inspected["NetworkSettings"], &settings); err != nil {
		return nil, err
	}
	var endpoints map[string]map[string]json.RawMessage
	if len(settings["Networks"]) == 0 {
		return document, nil
	}
	if err := json.Unmarshal(settings["Networks"], &endpoints); err != nil {
		return nil, err
	}
	names := []string{}
	for name, endpoint := range endpoints {
		var id string
		if len(endpoint["NetworkID"]) > 0 {
			if err := json.Unmarshal(endpoint["NetworkID"], &id); err != nil {
				return nil, fmt.Errorf("decode attached network identity: %w", err)
			}
		}
		if id == "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return document, nil
	}
	identities, err := m.containerNetworkIdentities(ctx, names)
	if err != nil {
		return nil, err
	}
	for name, id := range identities {
		if id == "" {
			continue
		}
		if endpoints[name] == nil {
			endpoints[name] = map[string]json.RawMessage{}
		}
		endpoints[name]["NetworkID"], err = json.Marshal(id)
		if err != nil {
			return nil, err
		}
	}
	settings["Networks"], err = json.Marshal(endpoints)
	if err != nil {
		return nil, err
	}
	inspected["NetworkSettings"], err = json.Marshal(settings)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inspected)
}

func normalizeCompatibilityInspectState(document json.RawMessage) (json.RawMessage, error) {
	var inspected map[string]json.RawMessage
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, fmt.Errorf("decode compatibility container inspection: %w", err)
	}
	if len(inspected["State"]) == 0 {
		return document, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(inspected["State"], &fields); err != nil {
		return nil, fmt.Errorf("decode compatibility container state: %w", err)
	}
	if string(fields["Status"]) != `""` {
		return document, nil
	}
	var observed struct {
		containerWaitState
		Paused     bool
		Restarting bool
		Dead       bool
	}
	if err := json.Unmarshal(inspected["State"], &observed); err != nil {
		return nil, fmt.Errorf("decode compatibility container lifecycle: %w", err)
	}
	status := observed.containerWaitState.normalized().Status
	switch {
	case observed.Dead:
		status = "dead"
	case observed.Restarting:
		status = "restarting"
	case observed.Paused:
		status = "paused"
	}
	var err error
	fields["Status"], err = json.Marshal(status)
	if err != nil {
		return nil, fmt.Errorf("encode compatibility container lifecycle: %w", err)
	}
	inspected["State"], err = json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encode compatibility container state: %w", err)
	}
	return json.Marshal(inspected)
}

func inspectOwnedContainerHealth(document json.RawMessage) (json.RawMessage, error) {
	var inspected map[string]json.RawMessage
	if err := json.Unmarshal(document, &inspected); err != nil {
		return nil, fmt.Errorf("decode owned container inspection: %w", err)
	}
	if len(inspected["Config"]) == 0 {
		return document, nil
	}
	var configuration map[string]json.RawMessage
	if err := json.Unmarshal(inspected["Config"], &configuration); err != nil {
		return nil, fmt.Errorf("decode container configuration: %w", err)
	}
	var labels map[string]string
	if data := configuration["Labels"]; len(data) > 0 {
		if err := json.Unmarshal(data, &labels); err != nil {
			return nil, fmt.Errorf("decode inspected container labels: %w", err)
		}
	}
	if labels[nerdctlHealthcheckLabel] == "" {
		return document, nil
	}
	container := Container{}
	if err := mapContainerHealth(&container, labels); err != nil {
		return nil, err
	}
	check := container.Healthcheck
	if err := validateHealthcheck(check); err != nil {
		return nil, err
	}
	var err error
	configuration["Healthcheck"], err = json.Marshal(map[string]any{
		"Test": check.Test, "Interval": int64(check.Interval), "Timeout": int64(check.Timeout),
		"StartPeriod": int64(check.StartPeriod), "StartInterval": int64(check.StartInterval), "Retries": check.Retries,
	})
	if err != nil {
		return nil, err
	}
	state := map[string]json.RawMessage{}
	if data := inspected["State"]; len(data) > 0 {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("decode inspected container state: %w", err)
		}
	}
	if state == nil {
		state = map[string]json.RawMessage{}
	}
	if container.Health.Status == "disabled" {
		delete(state, "Health")
	} else if ownsContainerHealth(labels) || len(state["Health"]) == 0 || string(state["Health"]) == "null" {
		if encoded := labels[nerdctlHealthStateLabel]; encoded != "" {
			state["Health"] = json.RawMessage(encoded)
		} else {
			state["Health"] = json.RawMessage(`{"Status":"starting","FailingStreak":0}`)
		}
	}
	inspected["Config"], err = json.Marshal(configuration)
	if err != nil {
		return nil, err
	}
	inspected["State"], err = json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return json.Marshal(inspected)
}
