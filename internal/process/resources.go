package process

import (
	"context"
	"fmt"

	"github.com/mbianchidev/porto/internal/resources"
)

type resourceRow struct {
	PID         int     `json:"pid"`
	ParentPID   int     `json:"parentPid"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemoryBytes int64   `json:"memoryBytes"`
}

func ResourceStats(ctx context.Context, pid int, includeChildren bool) (resources.Usage, error) {
	snapshot, err := CaptureResourceSnapshot(ctx)
	if err != nil {
		return resources.Usage{}, err
	}
	return snapshot.Stats(pid, includeChildren)
}

type ResourceSnapshot struct {
	rows []resourceRow
}

func CaptureResourceSnapshot(ctx context.Context) (ResourceSnapshot, error) {
	rows, err := readResourceRows(ctx)
	if err != nil {
		return ResourceSnapshot{}, err
	}
	return ResourceSnapshot{rows: rows}, nil
}

func (s ResourceSnapshot) Stats(pid int, includeChildren bool) (resources.Usage, error) {
	if pid <= 0 {
		return resources.Usage{}, errorsNewInvalidPID(pid)
	}
	byParent := make(map[int][]resourceRow)
	var root *resourceRow
	for index := range s.rows {
		row := s.rows[index]
		byParent[row.ParentPID] = append(byParent[row.ParentPID], row)
		if row.PID == pid {
			root = &s.rows[index]
		}
	}
	if root == nil {
		return resources.Usage{}, fmt.Errorf("process %d is not running", pid)
	}
	selected := []resourceRow{*root}
	if includeChildren {
		for index := 0; index < len(selected); index++ {
			selected = append(selected, byParent[selected[index].PID]...)
		}
	}
	var usage resources.Usage
	for _, row := range selected {
		usage.CPUMillicores += int64(row.CPUPercent*10 + 0.5)
		usage.MemoryBytes += row.MemoryBytes
	}
	return usage, nil
}

func errorsNewInvalidPID(pid int) error {
	return fmt.Errorf("invalid process ID %d", pid)
}
