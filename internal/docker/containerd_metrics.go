package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	cgroup1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroup2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/typeurl/v2"
	"github.com/mbianchidev/porto/internal/resources"
	"github.com/mbianchidev/porto/internal/runtimes"
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
	var response *tasksapi.MetricsResponse
	var backendCPU *resources.HostCPUUsage
	var backendRead time.Time
	if r.lima != "" {
		output, readErr := r.runRuntimeHelper(ctx, "task-metrics", "--address", r.address,
			"--namespace", r.namespace, "--id", container.ID)
		if readErr != nil {
			return ContainerMetricSample{}, readErr
		}
		var result struct {
			Metric  *apitypes.Metric       `json:"metric"`
			HostCPU resources.HostCPUUsage `json:"hostCPU"`
			Read    time.Time              `json:"read"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return ContainerMetricSample{}, fmt.Errorf("decode backend task/host CPU sample: %w", err)
		}
		if result.Metric.GetID() != container.ID || result.HostCPU.OnlineCPUs == 0 || result.Read.IsZero() {
			return ContainerMetricSample{}, errors.New("backend returned incomplete task/host CPU sample")
		}
		response = &tasksapi.MetricsResponse{Metrics: []*apitypes.Metric{result.Metric}}
		backendCPU, backendRead = &result.HostCPU, result.Read
	} else {
		response, err = r.tasks.Metrics(
			withContainerdNamespace(ctx, r.namespace),
			&tasksapi.MetricsRequest{Filters: []string{"id==" + container.ID}},
		)
	}
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
		if sample.CPU != nil {
			if backendCPU == nil {
				host, err := r.hostCPUUsage(ctx)
				if err != nil {
					return ContainerMetricSample{}, fmt.Errorf("%w: backend host CPU counters: %w", ErrUnavailable, err)
				}
				backendCPU = &host
			}
			sample.CPU.SystemCPUUsage = uint64Pointer(backendCPU.TotalUsage)
			sample.CPU.OnlineCPUs = backendCPU.OnlineCPUs
		}
		sample.ID = container.ID
		sample.Name = container.Name
		if !backendRead.IsZero() {
			sample.Read = backendRead
		} else if metric.GetTimestamp() != nil {
			sample.Read = metric.GetTimestamp().AsTime()
		} else {
			sample.Read = time.Now().UTC()
		}
		return sample, nil
	}
	return ContainerMetricSample{}, fmt.Errorf("%w: metrics for container %q", ErrNotFound, id)
}

func (r *grpcContainerRuntime) hostCPUUsage(ctx context.Context) (resources.HostCPUUsage, error) {
	if r.lima == "" {
		return resources.ReadHostCPUUsage()
	}
	output, err := r.runner.Run(ctx, runtimes.Command{
		Name: "limactl",
		Args: []string{"shell", "--workdir=/", r.lima, "--", "cat", "/proc/stat"},
	})
	if err != nil {
		return resources.HostCPUUsage{}, fmt.Errorf("read Lima backend CPU counters: %w", err)
	}
	return resources.ParseHostCPUUsage(output)
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
