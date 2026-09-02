//go:build !windows

package process

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func readResourceRows(ctx context.Context) ([]resourceRow, error) {
	command := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,%cpu=,rss=")
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("inspect process resources: %w: %s", err, strings.TrimSpace(string(output)))
	}
	rows := make([]resourceRow, 0)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parentPID, parentErr := strconv.Atoi(fields[1])
		cpu, cpuErr := strconv.ParseFloat(fields[2], 64)
		memoryKiB, memoryErr := strconv.ParseInt(fields[3], 10, 64)
		if pidErr != nil || parentErr != nil || cpuErr != nil || memoryErr != nil {
			return nil, fmt.Errorf("decode process resource row %q", line)
		}
		rows = append(rows, resourceRow{
			PID:         pid,
			ParentPID:   parentPID,
			CPUPercent:  cpu,
			MemoryBytes: memoryKiB * 1024,
		})
	}
	return rows, nil
}
