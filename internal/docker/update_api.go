package docker

import (
	"net/http"
	"strconv"
	"time"
)

func (a *API) updateContainer(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ContainerUpdate
		KernelMemory         int64  `json:"KernelMemory"`
		KernelMemoryTCP      int64  `json:"KernelMemoryTCP"`
		CPUCount             int64  `json:"CpuCount"`
		CPUPercent           int64  `json:"CpuPercent"`
		IOMaximumIOps        uint64 `json:"IOMaximumIOps"`
		IOMaximumBandwidth   uint64 `json:"IOMaximumBandwidth"`
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
		Healthcheck *struct {
			Test          []string `json:"Test"`
			Interval      int64    `json:"Interval"`
			Timeout       int64    `json:"Timeout"`
			StartPeriod   int64    `json:"StartPeriod"`
			StartInterval int64    `json:"StartInterval"`
			Retries       int      `json:"Retries"`
		} `json:"Healthcheck"`
	}
	if !decodeDockerOptionsJSON(w, r, &request) {
		return
	}
	if request.KernelMemory != 0 || request.KernelMemoryTCP != 0 ||
		request.CPUCount != 0 || request.CPUPercent != 0 ||
		request.IOMaximumIOps != 0 || request.IOMaximumBandwidth != 0 ||
		len(request.Ulimits) > 0 || len(request.Devices) > 0 ||
		len(request.DeviceCgroupRules) > 0 ||
		len(request.DeviceRequests) > 0 || len(request.BlkioWeightDevice) > 0 ||
		len(request.BlkioDeviceReadBps) > 0 || len(request.BlkioDeviceWriteBps) > 0 ||
		len(request.BlkioDeviceReadIOps) > 0 || len(request.BlkioDeviceWriteIOps) > 0 {
		writeDockerUnsupported(w, "non-updatable or platform-specific container resources")
		return
	}
	var healthcheck *ContainerHealthcheck
	if request.Healthcheck != nil {
		healthcheck = &ContainerHealthcheck{
			Test:          request.Healthcheck.Test,
			Interval:      time.Duration(request.Healthcheck.Interval),
			Timeout:       time.Duration(request.Healthcheck.Timeout),
			StartPeriod:   time.Duration(request.Healthcheck.StartPeriod),
			StartInterval: time.Duration(request.Healthcheck.StartInterval),
			Retries:       request.Healthcheck.Retries,
		}
	}
	restartPolicy := request.RestartPolicy.Name
	if request.RestartPolicy.MaximumRetryCount != 0 {
		if restartPolicy != "on-failure" {
			writeDockerUnsupported(w, "restart retry count without on-failure policy")
			return
		}
		restartPolicy += ":" + strconv.Itoa(request.RestartPolicy.MaximumRetryCount)
	}
	request.ContainerUpdate.Restart = restartPolicy
	request.ContainerUpdate.Healthcheck = healthcheck
	if err := a.manager.UpdateContainer(r.Context(), r.PathValue("id"), request.ContainerUpdate); err != nil {
		writeDockerError(w, err)
		return
	}
	writeDockerJSON(w, http.StatusOK, map[string]any{"Warnings": []string{}})
}
