package docker

import (
	"net/http"
)

func (a *API) updateContainer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Memory               int64  `json:"Memory"`
		MemorySwap           int64  `json:"MemorySwap"`
		NanoCPUs             int64  `json:"NanoCpus"`
		CPUShares            int64  `json:"CpuShares"`
		CPUPeriod            int64  `json:"CpuPeriod"`
		CPUQuota             int64  `json:"CpuQuota"`
		CPURealtimePeriod    int64  `json:"CpuRealtimePeriod"`
		CPURealtimeRuntime   int64  `json:"CpuRealtimeRuntime"`
		BlkioWeight          int64  `json:"BlkioWeight"`
		MemoryReservation    int64  `json:"MemoryReservation"`
		KernelMemory         int64  `json:"KernelMemory"`
		KernelMemoryTCP      int64  `json:"KernelMemoryTCP"`
		CPUSetCPUs           string `json:"CpusetCpus"`
		CPUSetMems           string `json:"CpusetMems"`
		CPUCount             int64  `json:"CpuCount"`
		CPUPercent           int64  `json:"CpuPercent"`
		IOMaximumIOps        uint64 `json:"IOMaximumIOps"`
		IOMaximumBandwidth   uint64 `json:"IOMaximumBandwidth"`
		MemorySwappiness     *int64 `json:"MemorySwappiness"`
		OomKillDisable       *bool  `json:"OomKillDisable"`
		PidsLimit            *int64 `json:"PidsLimit"`
		Ulimits              []any  `json:"Ulimits"`
		Devices              []any  `json:"Devices"`
		DeviceCgroupRules    []any  `json:"DeviceCgroupRules"`
		DeviceRequests       []any  `json:"DeviceRequests"`
		BlkioWeightDevice    []any  `json:"BlkioWeightDevice"`
		BlkioDeviceReadBps   []any  `json:"BlkioDeviceReadBps"`
		BlkioDeviceWriteBps  []any  `json:"BlkioDeviceWriteBps"`
		BlkioDeviceReadIOps  []any  `json:"BlkioDeviceReadIOps"`
		BlkioDeviceWriteIOps []any  `json:"BlkioDeviceWriteIOps"`
		RestartPolicy        struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
	}
	if !decodeDockerJSON(w, r, &request) {
		return
	}
	if request.CPUShares != 0 || request.CPUPeriod != 0 || request.CPUQuota != 0 ||
		request.CPURealtimePeriod != 0 || request.CPURealtimeRuntime != 0 ||
		request.BlkioWeight != 0 || request.MemoryReservation != 0 ||
		request.KernelMemory != 0 || request.KernelMemoryTCP != 0 ||
		request.CPUSetCPUs != "" || request.CPUSetMems != "" ||
		request.CPUCount != 0 || request.CPUPercent != 0 ||
		request.IOMaximumIOps != 0 || request.IOMaximumBandwidth != 0 ||
		request.MemorySwappiness != nil || request.OomKillDisable != nil ||
		request.PidsLimit != nil || len(request.Ulimits) > 0 || len(request.Devices) > 0 ||
		request.RestartPolicy.Name != "" || request.RestartPolicy.MaximumRetryCount != 0 ||
		len(request.DeviceCgroupRules) > 0 ||
		len(request.DeviceRequests) > 0 || len(request.BlkioWeightDevice) > 0 ||
		len(request.BlkioDeviceReadBps) > 0 || len(request.BlkioDeviceWriteBps) > 0 ||
		len(request.BlkioDeviceReadIOps) > 0 || len(request.BlkioDeviceWriteIOps) > 0 {
		writeDockerUnsupported(w, "container update options beyond CPU and memory limits")
		return
	}
	if err := a.manager.UpdateContainer(r.Context(), r.PathValue("id"), ContainerUpdate{
		Memory: request.Memory, MemorySwap: request.MemorySwap, NanoCPUs: request.NanoCPUs,
	}); err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
}
