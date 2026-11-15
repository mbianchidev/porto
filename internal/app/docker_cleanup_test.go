package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewDockerCleanupResultStartsEveryStepNotRun(t *testing.T) {
	result := NewDockerCleanupResult()
	for name, step := range map[string]DockerCleanupStep{
		"containers": result.Containers, "build cache": result.BuildCache, "images": result.Images,
		"volumes": result.Volumes, "host disk": result.HostDisk,
	} {
		if step.Status != CleanupNotRun || step.ItemsRemoved != 0 || step.BytesReclaimed != nil {
			t.Fatalf("new %s step = %+v, want an unrun step without counts", name, step)
		}
	}
}

func TestLegacyDockerCleanupResultsNormalizeMissingSteps(t *testing.T) {
	var result DockerCleanupResult
	if err := json.Unmarshal([]byte(`{"buildCache":{"status":"succeeded","itemsRemoved":3},"images":{"status":"failed","itemsRemoved":1}}`), &result); err != nil {
		t.Fatal(err)
	}
	result.Normalize()
	if result.Containers.Status != CleanupNotRun || result.Volumes.Status != CleanupNotRun || result.HostDisk.Status != CleanupNotRun ||
		result.BuildCache.Status != CleanupSucceeded || result.Images.Status != CleanupFailed {
		t.Fatalf("legacy cleanup result was not normalized without losing recorded steps: %+v", result)
	}
}

func TestDockerCleanupDurations(t *testing.T) {
	if DockerCleanupInterval != 24*time.Hour {
		t.Fatalf("cleanup interval = %s", DockerCleanupInterval)
	}
	if DockerCleanupTimeout != 20*time.Minute {
		t.Fatalf("cleanup timeout = %s", DockerCleanupTimeout)
	}
}

func TestDockerCleanupResultJSONOmitsOptionalEmptyFields(t *testing.T) {
	encoded, err := json.Marshal(NewDockerCleanupResult())
	if err != nil {
		t.Fatalf("marshal cleanup result: %v", err)
	}
	const expected = `{"containers":{"status":"not_run","itemsRemoved":0},"buildCache":{"status":"not_run","itemsRemoved":0},"images":{"status":"not_run","itemsRemoved":0},"volumes":{"status":"not_run","itemsRemoved":0},"hostDisk":{"status":"not_run","itemsRemoved":0}}`
	if string(encoded) != expected {
		t.Fatalf("cleanup result JSON = %s, want %s", encoded, expected)
	}
}

func TestDockerCleanupStepJSONIncludesExplicitZeroReclaimedBytes(t *testing.T) {
	reclaimed := int64(0)
	encoded, err := json.Marshal(DockerCleanupStep{
		Status:         CleanupSucceeded,
		ItemsRemoved:   2,
		BytesReclaimed: &reclaimed,
		Output:         "removed images",
	})
	if err != nil {
		t.Fatalf("marshal cleanup step: %v", err)
	}
	const expected = `{"status":"succeeded","itemsRemoved":2,"bytesReclaimed":0,"output":"removed images"}`
	if string(encoded) != expected {
		t.Fatalf("cleanup step JSON = %s, want %s", encoded, expected)
	}
}

func TestApplicationDefaults(t *testing.T) {
	if DefaultInterfaceDensity != "compact" {
		t.Fatalf("interface density = %q", DefaultInterfaceDensity)
	}
	if DefaultTerminalFontSize != 12 {
		t.Fatalf("terminal font size = %d", DefaultTerminalFontSize)
	}
	if DefaultTerminalLineHeight != 1.35 {
		t.Fatalf("terminal line height = %v", DefaultTerminalLineHeight)
	}
	if DefaultTerminalScrollback != 5000 {
		t.Fatalf("terminal scrollback = %d", DefaultTerminalScrollback)
	}
}
