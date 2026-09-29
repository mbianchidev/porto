package docker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type dockerEvent struct {
	Status   string           `json:"status"`
	ID       string           `json:"id"`
	From     string           `json:"from"`
	Type     string           `json:"Type"`
	Action   string           `json:"Action"`
	Actor    dockerEventActor `json:"Actor"`
	Scope    string           `json:"scope"`
	Time     int64            `json:"time"`
	TimeNano int64            `json:"timeNano"`
}

type dockerEventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	filters, err := parseDockerFilters(r.URL.Query().Get("filters"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	since, sinceSet, err := parseDockerEventTime(r.URL.Query().Get("since"))
	if err != nil {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	until, untilSet, err := parseDockerEventTime(r.URL.Query().Get("until"))
	if err != nil {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}
	if sinceSet && untilSet && until.Before(since) {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "event until time precedes since time"})
		return
	}
	updates, initial, unsubscribe := a.manager.SubscribeContainerSnapshotsWithInitial()
	defer unsubscribe()
	if initial.InstanceID == "inactive" && !initial.Available {
		writeDockerError(w, fmt.Errorf("%w: container event inventory is not running", ErrUnavailable))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Porto-Event-Replay-Limit", strconv.Itoa(maxInventoryEvents))
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	flusher, _ := w.(http.Flusher)
	lastSequence := latestEventSequence(initial.Events)
	if sinceSet {
		for _, event := range initial.Events {
			if !eventWithinBounds(event, since, true, until, untilSet) {
				continue
			}
			if encoded, ok := dockerEventFromLifecycle(initial, event); ok &&
				dockerEventMatches(encoded, filters) {
				if err := encoder.Encode(encoded); err != nil {
					return
				}
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	if untilSet && !time.Now().Before(until) {
		return
	}
	var untilChannel <-chan time.Time
	var timer *time.Timer
	if untilSet {
		timer = time.NewTimer(time.Until(until))
		defer timer.Stop()
		untilChannel = timer.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-untilChannel:
			return
		case snapshot, ok := <-updates:
			if !ok {
				return
			}
			for _, event := range snapshot.Events {
				if event.Sequence <= lastSequence {
					continue
				}
				lastSequence = event.Sequence
				if !eventWithinBounds(event, since, sinceSet, until, untilSet) {
					continue
				}
				encoded, ok := dockerEventFromLifecycle(snapshot, event)
				if !ok || !dockerEventMatches(encoded, filters) {
					continue
				}
				if err := encoder.Encode(encoded); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

func parseDockerEventTime(value string) (time.Time, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false, nil
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		whole := int64(seconds)
		nanoseconds := int64((seconds - float64(whole)) * float64(time.Second))
		return time.Unix(whole, nanoseconds).UTC(), true, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("invalid Docker event time %q", value)
	}
	return parsed.UTC(), true, nil
}

func latestEventSequence(events []ContainerLifecycleEvent) uint64 {
	var sequence uint64
	for _, event := range events {
		if event.Sequence > sequence {
			sequence = event.Sequence
		}
	}
	return sequence
}

func eventWithinBounds(
	event ContainerLifecycleEvent,
	since time.Time,
	sinceSet bool,
	until time.Time,
	untilSet bool,
) bool {
	if sinceSet && event.Timestamp.Before(since) {
		return false
	}
	if untilSet && event.Timestamp.After(until) {
		return false
	}
	return true
}

func dockerEventFromLifecycle(snapshot ContainerSnapshot, event ContainerLifecycleEvent) (dockerEvent, bool) {
	action, ok := dockerEventAction(event)
	if !ok || event.ContainerID == "" {
		return dockerEvent{}, false
	}
	attributes := map[string]string{}
	image := ""
	for _, container := range snapshot.Containers {
		if container.ID != event.ContainerID {
			continue
		}
		if container.Name != "" {
			attributes["name"] = container.Name
		}
		image = container.Image
		if image != "" {
			attributes["image"] = image
		}
		for key, value := range container.Labels {
			attributes[key] = value
		}
		break
	}
	if event.ExecID != "" {
		attributes["execID"] = event.ExecID
	}
	if event.ExitCode != nil {
		attributes["exitCode"] = strconv.FormatUint(uint64(*event.ExitCode), 10)
	}
	if event.ExitSignal != nil {
		attributes["signal"] = strconv.FormatUint(uint64(*event.ExitSignal), 10)
	}
	if event.Reason != "" {
		attributes["reason"] = event.Reason
	}
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return dockerEvent{
		Status: action,
		ID:     event.ContainerID,
		From:   image,
		Type:   "container",
		Action: action,
		Actor: dockerEventActor{
			ID:         event.ContainerID,
			Attributes: attributes,
		},
		Scope:    "local",
		Time:     timestamp.Unix(),
		TimeNano: timestamp.UnixNano(),
	}, true
}

func dockerEventAction(event ContainerLifecycleEvent) (string, bool) {
	switch event.Type {
	case "container-create":
		return "create", true
	case "container-update", "metadata-update", "network-update", "resource-update":
		return "update", true
	case "container-delete":
		return "destroy", true
	case "task-start":
		return "start", true
	case "task-exit":
		return "die", true
	case "task-oom":
		return "oom", true
	case "task-paused":
		return "pause", true
	case "task-resumed":
		return "unpause", true
	case "task-checkpointed":
		return "checkpoint", true
	case "exec-added":
		return "exec_create", true
	case "exec-started":
		return "exec_start", true
	case "exec-exit", "exec-delete":
		return "exec_die", true
	case "restart":
		return "restart", true
	case "health-transition":
		return "health_status: " + transitionTarget(event.Reason), true
	case "state-transition":
		return transitionTarget(event.Reason), transitionTarget(event.Reason) != ""
	default:
		return "", false
	}
}

func transitionTarget(reason string) string {
	_, target, ok := strings.Cut(reason, "->")
	if !ok {
		return ""
	}
	return strings.TrimSpace(target)
}

func dockerEventMatches(event dockerEvent, filters dockerFilters) bool {
	if !filters.matchesValue("type", event.Type) ||
		!filters.matchesValue("event", event.Action) ||
		!filters.matchesValue("scope", event.Scope) ||
		!filters.matchesLabels(event.Actor.Attributes) {
		return false
	}
	if values := filters["container"]; len(values) > 0 {
		name := event.Actor.Attributes["name"]
		matched := false
		for candidate, enabled := range values {
			if enabled && (strings.HasPrefix(event.ID, candidate) || strings.Contains(name, strings.Trim(candidate, "^$"))) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if values := filters["image"]; len(values) > 0 {
		matched := false
		for candidate, enabled := range values {
			if enabled && (event.From == candidate || strings.Contains(event.From, candidate)) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
