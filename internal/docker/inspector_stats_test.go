package docker

import (
	"context"
	"testing"
	"time"
)

func TestInspectorStatsBoundsHistoryAndDoesNotInventMissingCounters(t *testing.T) {
	manager := New(&fakeRunner{})
	var index int
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		index++
		return ContainerMetricSample{
			ID: "synthetic-container", Read: time.Unix(int64(index*2), 0),
			CPU: &ContainerMetricCPU{TotalUsage: uint64(index) * 1000000000},
		}, nil
	}
	var stats InspectorStats
	for range maxInspectorStats + 20 {
		var err error
		stats, err = manager.InspectorStats(context.Background(), "synthetic-container")
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(stats.History) != maxInspectorStats {
		t.Fatalf("unbounded history: %d", len(stats.History))
	}
	if stats.Current == nil || stats.Current.CPUMillicores == nil || *stats.Current.CPUMillicores != 500 {
		t.Fatalf("incorrect CPU delta: %+v", stats.Current)
	}
	if stats.Current.MemoryBytes != nil || stats.Current.NetworkRX != nil || stats.Current.BlockRead != nil {
		t.Fatal("unavailable counters were reported as zero")
	}
}
