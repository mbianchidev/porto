package resources

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

type HostCPUUsage struct {
	TotalUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs uint32 `json:"online_cpus"`
}

func ReadHostCPUUsage() (HostCPUUsage, error) {
	if runtime.GOOS != "linux" {
		return HostCPUUsage{}, errors.New("backend host CPU counters require Linux /proc/stat")
	}
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return HostCPUUsage{}, fmt.Errorf("read backend host CPU counters: %w", err)
	}
	return ParseHostCPUUsage(data)
}

func ParseHostCPUUsage(data []byte) (HostCPUUsage, error) {
	var result HostCPUUsage
	var total uint64
	aggregate := false
	cpus := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			if _, err := strconv.ParseUint(strings.TrimPrefix(fields[0], "cpu"), 10, 32); err != nil {
				return result, fmt.Errorf("invalid backend CPU identifier %q", fields[0])
			}
			if cpus[fields[0]] {
				return result, fmt.Errorf("duplicate backend CPU identifier %q", fields[0])
			}
			cpus[fields[0]] = true
			continue
		}
		if aggregate || len(fields) < 5 {
			return result, errors.New("invalid aggregate backend CPU counters")
		}
		aggregate = true
		// Linux /proc/stat uses USER_HZ (100 on the supported amd64/arm64 backends).
		// Guest counters already contribute to user/nice and must not be counted twice.
		for _, field := range fields[1:min(len(fields), 9)] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil || value > ^uint64(0)-total {
				return result, errors.New("invalid or overflowing backend CPU counters")
			}
			total += value
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scan backend CPU counters: %w", err)
	}
	const nanosecondsPerTick = uint64(10_000_000)
	if !aggregate || len(cpus) == 0 || total > ^uint64(0)/nanosecondsPerTick {
		return result, errors.New("backend CPU totals or online CPU count are unavailable")
	}
	result.TotalUsage = total * nanosecondsPerTick
	result.OnlineCPUs = uint32(len(cpus))
	return result, nil
}
