package docker

import (
	"context"
	"errors"
	"strings"
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

func TestInspectorStatsPreservesCancellationWithCachedHistory(t *testing.T) {
	manager := New(&fakeRunner{})
	manager.metricReader = func(ctx context.Context, id string) (ContainerMetricSample, error) {
		if err := ctx.Err(); err != nil {
			return ContainerMetricSample{}, err
		}
		return ContainerMetricSample{ID: id, Read: time.Unix(2, 0)}, nil
	}
	if _, err := manager.InspectorStats(context.Background(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.InspectorStats(ctx, "synthetic"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request returned cached success: %v", err)
	}
}

func TestInspectorStatsReportsConnectionLossAndCounterReset(t *testing.T) {
	manager := New(&fakeRunner{})
	var index int
	manager.metricReader = func(context.Context, string) (ContainerMetricSample, error) {
		index++
		if index == 4 {
			return ContainerMetricSample{}, errors.New("synthetic connection lost")
		}
		usage := uint64(index) * 1_000_000_000
		if index == 3 {
			usage = 1
		}
		return ContainerMetricSample{
			ID: "synthetic", Read: time.Unix(int64(index*2), 0),
			CPU:      &ContainerMetricCPU{TotalUsage: usage},
			Memory:   &ContainerMetricMemory{Usage: metricUint64(1024), Limit: metricUint64(4096)},
			PIDs:     &ContainerMetricPIDs{Current: 2},
			Networks: map[string]ContainerMetricNetwork{"eth0": {RxBytes: 10, TxBytes: 20}, "eth1": {RxBytes: 30, TxBytes: 40}},
			BlockIO:  []ContainerMetricBlockIO{{Op: "Read", Value: 50}, {Op: "write", Value: 60}},
		}, nil
	}
	for range 3 {
		if _, err := manager.InspectorStats(context.Background(), "synthetic"); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := manager.InspectorStats(context.Background(), "synthetic")
	if err != nil || stats.Available || len(stats.History) != 3 || !strings.Contains(stats.Message, "synthetic connection lost") {
		t.Fatalf("connection-loss state = %+v, error = %v", stats, err)
	}
	point := stats.Current
	if point == nil || point.CPUMillicores != nil {
		t.Fatalf("counter reset invented CPU usage: %+v", point)
	}
	if *point.MemoryBytes != 1024 || *point.MemoryLimit != 4096 || *point.PIDs != 2 ||
		*point.NetworkRX != 40 || *point.NetworkTX != 60 || *point.BlockRead != 50 || *point.BlockWrite != 60 {
		t.Fatalf("incorrect retained counters: %+v", point)
	}
}
