package docker

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func (limits ContainerUpdate) hasResources() bool {
	limits.Restart = ""
	limits.Healthcheck = nil
	return !reflect.DeepEqual(limits, ContainerUpdate{})
}

func (limits ContainerUpdate) validate() (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("invalid container resource limits: %w", err)
		}
	}()
	if limits.Memory < 0 || limits.MemoryReservation < 0 || limits.MemorySwap < -1 ||
		limits.NanoCPUs < 0 || limits.CPUShares < 0 || limits.CPUPeriod < 0 ||
		limits.CPUQuota < -1 || limits.CPURealtimePeriod < 0 || limits.CPURealtimeRuntime < -1 {
		return errors.New("invalid negative container resource limit")
	}
	if limits.NanoCPUs != 0 && (limits.CPUPeriod != 0 || limits.CPUQuota != 0) {
		return errors.New("NanoCpus cannot be combined with CpuPeriod or CpuQuota")
	}
	if limits.NanoCPUs > 0 && limits.NanoCPUs < 10_000_000 {
		return errors.New("NanoCpus must represent at least 0.01 CPU")
	}
	if limits.CPUShares != 0 && (limits.CPUShares < 2 || limits.CPUShares > 262144) {
		return errors.New("CpuShares must be between 2 and 262144")
	}
	if limits.CPUPeriod != 0 && (limits.CPUPeriod < 1000 || limits.CPUPeriod > 1000000) {
		return errors.New("CpuPeriod must be between 1000 and 1000000")
	}
	if limits.CPUQuota > 0 && limits.CPUQuota < 1000 {
		return errors.New("CpuQuota must be at least 1000 or -1 for unlimited")
	}
	if limits.MemorySwap > 0 && limits.Memory > 0 && limits.MemorySwap < limits.Memory {
		return errors.New("MemorySwap must be at least Memory")
	}
	if limits.PidsLimit != nil && *limits.PidsLimit < -1 {
		return errors.New("PidsLimit must be positive, zero, or -1 for unlimited")
	}
	if limits.MemorySwappiness != nil && (*limits.MemorySwappiness < -1 || *limits.MemorySwappiness > 100) {
		return errors.New("MemorySwappiness must be between 0 and 100, or -1")
	}
	if limits.BlkioWeight != 0 && (limits.BlkioWeight < 10 || limits.BlkioWeight > 1000) {
		return errors.New("BlkioWeight must be between 10 and 1000")
	}
	for _, set := range []string{limits.CPUSetCPUs, limits.CPUSetMems} {
		for _, value := range strings.Split(set, ",") {
			if value == "" && set == "" {
				continue
			}
			bounds := strings.Split(value, "-")
			if len(bounds) > 2 {
				return fmt.Errorf("invalid CPU set %q", set)
			}
			first, err := strconv.ParseUint(bounds[0], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid CPU set %q", set)
			}
			if len(bounds) == 2 {
				last, err := strconv.ParseUint(bounds[1], 10, 32)
				if err != nil || last < first {
					return fmt.Errorf("invalid CPU set %q", set)
				}
			}
		}
	}
	return nil
}

func (limits ContainerUpdate) arguments(args []string) []string {
	if limits.NanoCPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(limits.NanoCPUs)/1e9, 'f', -1, 64))
	}
	for _, option := range []struct {
		name  string
		value int64
	}{
		{"--memory", limits.Memory}, {"--memory-swap", limits.MemorySwap},
		{"--memory-reservation", limits.MemoryReservation}, {"--cpu-shares", limits.CPUShares},
		{"--cpu-period", limits.CPUPeriod}, {"--cpu-quota", limits.CPUQuota},
		{"--cpu-rt-period", limits.CPURealtimePeriod}, {"--cpu-rt-runtime", limits.CPURealtimeRuntime},
		{"--blkio-weight", int64(limits.BlkioWeight)},
	} {
		if option.value != 0 {
			args = append(args, option.name, strconv.FormatInt(option.value, 10))
		}
	}
	args = appendStringFlag(args, "--cpuset-cpus", limits.CPUSetCPUs)
	args = appendStringFlag(args, "--cpuset-mems", limits.CPUSetMems)
	if limits.PidsLimit != nil {
		args = append(args, "--pids-limit", strconv.FormatInt(*limits.PidsLimit, 10))
	}
	if limits.MemorySwappiness != nil && *limits.MemorySwappiness != -1 {
		args = append(args, "--memory-swappiness", strconv.FormatInt(*limits.MemorySwappiness, 10))
	}
	if limits.OomKillDisable != nil {
		args = append(args, "--oom-kill-disable="+strconv.FormatBool(*limits.OomKillDisable))
	}
	return args
}
