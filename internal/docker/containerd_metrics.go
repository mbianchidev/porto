package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cgroup1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroup2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/typeurl/v2"
)

func (r *grpcContainerRuntime) ContainerMetric(
	ctx context.Context,
	id string,
) (ContainerMetricSample, error) {
	if r.tasks == nil {
		return ContainerMetricSample{}, fmt.Errorf("%w: containerd task metrics", ErrUnsupported)
	}
	containers, err := r.Snapshot(ctx)
	if err != nil {
		return ContainerMetricSample{}, err
	}
	container, err := findSnapshotContainer(ContainerSnapshot{Containers: containers}, id)
	if err != nil {
		return ContainerMetricSample{}, err
	}
	if !containerActive(container) {
		return ContainerMetricSample{}, fmt.Errorf("%w: container %q is not running", ErrConflict, id)
	}
	response, err := r.tasks.Metrics(
		withContainerdNamespace(ctx, r.namespace),
		&tasksapi.MetricsRequest{Filters: []string{"id==" + container.ID}},
	)
	if err != nil {
		return ContainerMetricSample{}, containerdOperationError("read metrics for", container.ID, err)
	}
	for _, metric := range response.GetMetrics() {
		if metric.GetID() != container.ID {
			continue
		}
		if metric.GetData() == nil {
			return ContainerMetricSample{}, errors.New("containerd returned empty task metrics")
		}
		decoded, err := typeurl.UnmarshalAny(metric.GetData())
		if err != nil {
			return ContainerMetricSample{}, fmt.Errorf("decode container metrics: %w", err)
		}
		var sample ContainerMetricSample
		switch value := decoded.(type) {
		case *cgroup1.Metrics:
			sample, err = containerMetricFromCgroup1(value)
		case *cgroup2.Metrics:
			sample, err = containerMetricFromCgroup2(value)
		default:
			err = fmt.Errorf("unsupported container metrics type %T", decoded)
		}
		if err != nil {
			return ContainerMetricSample{}, err
		}
		sample.ID = container.ID
		sample.Name = container.Name
		if metric.GetTimestamp() != nil {
			sample.Read = metric.GetTimestamp().AsTime()
		} else {
			sample.Read = time.Now().UTC()
		}
		return sample, nil
	}
	return ContainerMetricSample{}, fmt.Errorf("%w: metrics for container %q", ErrNotFound, id)
}

func containerMetricFromCgroup1(metrics *cgroup1.Metrics) (ContainerMetricSample, error) {
	sample := ContainerMetricSample{}
	if metrics == nil {
		return sample, errors.New("containerd returned nil cgroup v1 metrics")
	}
	if cpu := metrics.GetCPU(); cpu != nil {
		usage := cpu.GetUsage()
		throttling := cpu.GetThrottling()
		if usage != nil {
			sample.CPU = &ContainerMetricCPU{
				TotalUsage:  usage.GetTotal(),
				UserUsage:   usage.GetUser(),
				SystemUsage: usage.GetKernel(),
				PerCPUUsage: append([]uint64(nil), usage.GetPerCPU()...),
			}
			if throttling != nil {
				sample.CPU.Throttling = ContainerMetricThrottling{
					Periods:          throttling.GetPeriods(),
					ThrottledPeriods: throttling.GetThrottledPeriods(),
					ThrottledTime:    throttling.GetThrottledTime(),
				}
			}
		}
	}
	if memory := metrics.GetMemory(); memory != nil {
		usage := memory.GetUsage()
		sample.Memory = &ContainerMetricMemory{
			Stats: map[string]uint64{
				"cache":               memory.GetCache(),
				"rss":                 memory.GetRSS(),
				"rss_huge":            memory.GetRSSHuge(),
				"mapped_file":         memory.GetMappedFile(),
				"pgfault":             memory.GetPgFault(),
				"pgmajfault":          memory.GetPgMajFault(),
				"inactive_anon":       memory.GetInactiveAnon(),
				"active_anon":         memory.GetActiveAnon(),
				"inactive_file":       memory.GetInactiveFile(),
				"active_file":         memory.GetActiveFile(),
				"total_cache":         memory.GetTotalCache(),
				"total_rss":           memory.GetTotalRSS(),
				"total_inactive_file": memory.GetTotalInactiveFile(),
			},
		}
		if usage != nil {
			sample.Memory.Usage = uint64Pointer(usage.GetUsage())
			sample.Memory.MaxUsage = uint64Pointer(usage.GetMax())
			sample.Memory.Limit = uint64Pointer(usage.GetLimit())
		}
	}
	if pids := metrics.GetPids(); pids != nil {
		sample.PIDs = &ContainerMetricPIDs{Current: pids.GetCurrent(), Limit: pids.GetLimit()}
	}
	for _, network := range metrics.GetNetwork() {
		if network == nil || network.GetName() == "" {
			continue
		}
		if sample.Networks == nil {
			sample.Networks = make(map[string]ContainerMetricNetwork)
		}
		sample.Networks[network.GetName()] = ContainerMetricNetwork{
			RxBytes: network.GetRxBytes(), RxPackets: network.GetRxPackets(),
			RxErrors: network.GetRxErrors(), RxDropped: network.GetRxDropped(),
			TxBytes: network.GetTxBytes(), TxPackets: network.GetTxPackets(),
			TxErrors: network.GetTxErrors(), TxDropped: network.GetTxDropped(),
		}
	}
	if block := metrics.GetBlkio(); block != nil {
		for _, entry := range block.GetIoServiceBytesRecursive() {
			if entry == nil {
				continue
			}
			sample.BlockIO = append(sample.BlockIO, ContainerMetricBlockIO{
				Major: entry.GetMajor(), Minor: entry.GetMinor(),
				Op: dockerBlockOperation(entry.GetOp()), Value: entry.GetValue(),
			})
		}
	}
	return sample, nil
}

func containerMetricFromCgroup2(metrics *cgroup2.Metrics) (ContainerMetricSample, error) {
	sample := ContainerMetricSample{}
	if metrics == nil {
		return sample, errors.New("containerd returned nil cgroup v2 metrics")
	}
	if cpu := metrics.GetCPU(); cpu != nil {
		total, err := microsecondsToNanoseconds(cpu.GetUsageUsec())
		if err != nil {
			return sample, err
		}
		user, err := microsecondsToNanoseconds(cpu.GetUserUsec())
		if err != nil {
			return sample, err
		}
		system, err := microsecondsToNanoseconds(cpu.GetSystemUsec())
		if err != nil {
			return sample, err
		}
		throttled, err := microsecondsToNanoseconds(cpu.GetThrottledUsec())
		if err != nil {
			return sample, err
		}
		sample.CPU = &ContainerMetricCPU{
			TotalUsage: total, UserUsage: user, SystemUsage: system,
			Throttling: ContainerMetricThrottling{
				Periods:          cpu.GetNrPeriods(),
				ThrottledPeriods: cpu.GetNrThrottled(),
				ThrottledTime:    throttled,
			},
		}
	}
	if memory := metrics.GetMemory(); memory != nil {
		sample.Memory = &ContainerMetricMemory{
			Usage: uint64Pointer(memory.GetUsage()), MaxUsage: uint64Pointer(memory.GetMaxUsage()),
			Limit: uint64Pointer(memory.GetUsageLimit()),
			Stats: map[string]uint64{
				"anon":               memory.GetAnon(),
				"file":               memory.GetFile(),
				"kernel_stack":       memory.GetKernelStack(),
				"slab":               memory.GetSlab(),
				"sock":               memory.GetSock(),
				"shmem":              memory.GetShmem(),
				"file_mapped":        memory.GetFileMapped(),
				"file_dirty":         memory.GetFileDirty(),
				"file_writeback":     memory.GetFileWriteback(),
				"inactive_anon":      memory.GetInactiveAnon(),
				"active_anon":        memory.GetActiveAnon(),
				"inactive_file":      memory.GetInactiveFile(),
				"active_file":        memory.GetActiveFile(),
				"unevictable":        memory.GetUnevictable(),
				"pgfault":            memory.GetPgfault(),
				"pgmajfault":         memory.GetPgmajfault(),
				"workingset_refault": memory.GetWorkingsetRefault(),
			},
		}
	}
	if pids := metrics.GetPids(); pids != nil {
		sample.PIDs = &ContainerMetricPIDs{Current: pids.GetCurrent(), Limit: pids.GetLimit()}
	}
	for _, network := range metrics.GetNetwork() {
		if network == nil || network.GetName() == "" {
			continue
		}
		if sample.Networks == nil {
			sample.Networks = make(map[string]ContainerMetricNetwork)
		}
		sample.Networks[network.GetName()] = ContainerMetricNetwork{
			RxBytes: network.GetRxBytes(), RxPackets: network.GetRxPackets(),
			RxErrors: network.GetRxErrors(), RxDropped: network.GetRxDropped(),
			TxBytes: network.GetTxBytes(), TxPackets: network.GetTxPackets(),
			TxErrors: network.GetTxErrors(), TxDropped: network.GetTxDropped(),
		}
	}
	if ioStats := metrics.GetIo(); ioStats != nil {
		for _, entry := range ioStats.GetUsage() {
			if entry == nil {
				continue
			}
			sample.BlockIO = append(sample.BlockIO,
				ContainerMetricBlockIO{Major: entry.GetMajor(), Minor: entry.GetMinor(), Op: "Read", Value: entry.GetRbytes()},
				ContainerMetricBlockIO{Major: entry.GetMajor(), Minor: entry.GetMinor(), Op: "Write", Value: entry.GetWbytes()},
			)
		}
	}
	return sample, nil
}

func microsecondsToNanoseconds(value uint64) (uint64, error) {
	if value > ^uint64(0)/1000 {
		return 0, errors.New("container CPU metrics overflow nanosecond counters")
	}
	return value * 1000, nil
}

func dockerBlockOperation(operation string) string {
	switch strings.ToLower(operation) {
	case "read":
		return "Read"
	case "write":
		return "Write"
	case "sync":
		return "Sync"
	case "async":
		return "Async"
	case "total":
		return "Total"
	default:
		return operation
	}
}

func uint64Pointer(value uint64) *uint64 {
	return &value
}
