package docker

import (
	"encoding/json"
	"fmt"
)

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
