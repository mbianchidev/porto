package app

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewDockerCleanupResultStartsEveryStepNotRun(t *testing.T) {
	result := NewDockerCleanupResult()

	if result.BuildCache.Status != CleanupNotRun {
		t.Fatalf("build cache status = %q, want %q", result.BuildCache.Status, CleanupNotRun)
	}
	if result.Images.Status != CleanupNotRun {
		t.Fatalf("image status = %q, want %q", result.Images.Status, CleanupNotRun)
	}
	if result.BuildCache.ItemsRemoved != 0 || result.Images.ItemsRemoved != 0 {
		t.Fatalf("new cleanup result has removed items: %+v", result)
	}
	if result.BuildCache.BytesReclaimed != nil || result.Images.BytesReclaimed != nil {
		t.Fatalf("new cleanup result has reclaimed byte counts: %+v", result)
	}
}

func TestDockerCleanupDurations(t *testing.T) {
	if DockerCleanupInterval != 7*24*time.Hour {
		t.Fatalf("cleanup interval = %s", DockerCleanupInterval)
	}
	if DockerCleanupTimeout != 10*time.Minute {
		t.Fatalf("cleanup timeout = %s", DockerCleanupTimeout)
	}
}

func TestDockerCleanupResultJSONOmitsOptionalEmptyFields(t *testing.T) {
	encoded, err := json.Marshal(NewDockerCleanupResult())
	if err != nil {
		t.Fatalf("marshal cleanup result: %v", err)
	}
	const expected = `{"buildCache":{"status":"not_run","itemsRemoved":0},"images":{"status":"not_run","itemsRemoved":0}}`
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
