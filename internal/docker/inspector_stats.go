package docker

import (
	"context"
	"time"
)

const maxInspectorStats = 180

type InspectorStatsPoint struct {
	Read          time.Time `json:"read"`
	CPUMillicores *int64    `json:"cpuMillicores,omitempty"`
	MemoryBytes   *uint64   `json:"memoryBytes,omitempty"`
	MemoryLimit   *uint64   `json:"memoryLimit,omitempty"`
	PIDs          *uint64   `json:"pids,omitempty"`
	NetworkRX     *uint64   `json:"networkRX,omitempty"`
	NetworkTX     *uint64   `json:"networkTX,omitempty"`
	BlockRead     *uint64   `json:"blockRead,omitempty"`
	BlockWrite    *uint64   `json:"blockWrite,omitempty"`
	CPUUsage      *uint64   `json:"-"`
}

type InspectorStats struct {
	Available bool                  `json:"available"`
	Current   *InspectorStatsPoint  `json:"current,omitempty"`
	History   []InspectorStatsPoint `json:"history"`
	Message   string                `json:"message,omitempty"`
}

func (m *Manager) InspectorStats(ctx context.Context, id string) (InspectorStats, error) {
	sample, err := m.ContainerMetric(ctx, id)
	if err != nil {
		m.metricHistoryMu.Lock()
		history := append([]InspectorStatsPoint(nil), m.metricHistory[id]...)
		m.metricHistoryMu.Unlock()
		if len(history) > 0 {
			last := history[len(history)-1]
			return InspectorStats{Available: false, Current: &last, History: history, Message: "Live counters are unavailable: " + err.Error()}, nil
		}
		return InspectorStats{}, err
	}
	point := inspectorStatsPoint(sample)
	m.metricHistoryMu.Lock()
	defer m.metricHistoryMu.Unlock()
	if m.metricHistory == nil {
		m.metricHistory = make(map[string][]InspectorStatsPoint)
	}
	history := m.metricHistory[sample.ID]
	if len(history) > 0 {
		previous := history[len(history)-1]
		elapsed := point.Read.Sub(previous.Read)
		if point.CPUUsage != nil && previous.CPUUsage != nil && *point.CPUUsage >= *previous.CPUUsage && elapsed > 0 {
			millicores := int64(float64(*point.CPUUsage-*previous.CPUUsage) * 1000 / float64(elapsed))
			point.CPUMillicores = &millicores
		}
		if elapsed < time.Second {
			return InspectorStats{Available: true, Current: &previous, History: append([]InspectorStatsPoint(nil), history...)}, nil
		}
	}
	history = append(history, point)
	if len(history) > maxInspectorStats {
		history = history[len(history)-maxInspectorStats:]
	}
	if len(m.metricHistory) >= 128 && len(m.metricHistory[sample.ID]) == 0 {
		var oldest string
		var oldestAt time.Time
		for key, points := range m.metricHistory {
			if len(points) > 0 && (oldestAt.IsZero() || points[len(points)-1].Read.Before(oldestAt)) {
				oldest, oldestAt = key, points[len(points)-1].Read
			}
		}
		delete(m.metricHistory, oldest)
	}
	m.metricHistory[sample.ID] = history
	return InspectorStats{
		Available: true, Current: &point, History: append([]InspectorStatsPoint(nil), history...),
		Message: "Recent samples are bounded to 180 points per container and collected while this view is open. Missing backend counters are unavailable, not zero.",
	}, nil
}

func inspectorStatsPoint(sample ContainerMetricSample) InspectorStatsPoint {
	point := InspectorStatsPoint{Read: sample.Read}
	if sample.CPU != nil {
		usage := sample.CPU.TotalUsage
		point.CPUUsage = &usage
	}
	if sample.Memory != nil {
		point.MemoryBytes, point.MemoryLimit = sample.Memory.Usage, sample.Memory.Limit
	}
	if sample.PIDs != nil {
		current := sample.PIDs.Current
		point.PIDs = &current
	}
	if len(sample.Networks) > 0 {
		var rx, tx uint64
		for _, network := range sample.Networks {
			rx += network.RxBytes
			tx += network.TxBytes
		}
		point.NetworkRX, point.NetworkTX = &rx, &tx
	}
	if len(sample.BlockIO) > 0 {
		var read, written uint64
		for _, block := range sample.BlockIO {
			switch block.Op {
			case "Read", "read":
				read += block.Value
			case "Write", "write":
				written += block.Value
			}
		}
		point.BlockRead, point.BlockWrite = &read, &written
	}
	return point
}
