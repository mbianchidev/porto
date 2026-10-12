package docker

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

func (a *API) containerStats(w http.ResponseWriter, r *http.Request) {
	stream := true
	if value := r.URL.Query().Get("stream"); value != "" {
		var err error
		stream, err = strconv.ParseBool(value)
		if err != nil {
			writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid stats stream option"})
			return
		}
	}
	oneShot := false
	if value := r.URL.Query().Get("one-shot"); value != "" {
		var err error
		oneShot, err = strconv.ParseBool(value)
		if err != nil {
			writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid stats one-shot option"})
			return
		}
	}
	if stream && oneShot {
		writeDockerJSON(w, http.StatusBadRequest, map[string]string{"message": "one-shot is only supported with stream=false"})
		return
	}
	current, err := a.manager.ContainerMetric(r.Context(), r.PathValue("id"))
	if err != nil {
		writeDockerError(w, err)
		return
	}
	var previousSample *ContainerMetricSample
	if !stream && !oneShot {
		previous := current
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
		}
		current, err = a.manager.ContainerMetric(r.Context(), r.PathValue("id"))
		if err != nil {
			writeDockerError(w, err)
			return
		}
		previousSample = &previous
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	if err := encoder.Encode(dockerStatsDocument(current, previousSample)); err != nil {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if !stream {
		return
	}
	previous := current
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			current, err = a.manager.ContainerMetric(r.Context(), r.PathValue("id"))
			if err != nil {
				log.Printf("Docker stats stream failed for %s: %v", r.PathValue("id"), err)
				_ = encodeDockerProgress(w, encoder, map[string]any{
					"error":       err.Error(),
					"errorDetail": map[string]string{"message": err.Error()},
				})
				return
			}
			if err := encoder.Encode(dockerStatsDocument(current, &previous)); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			previous = current
		}
	}
}

func dockerStatsDocument(current ContainerMetricSample, previous *ContainerMetricSample) map[string]any {
	name := current.Name
	if name != "" && name[0] != '/' {
		name = "/" + name
	}
	document := map[string]any{
		"read": current.Read.UTC().Format(time.RFC3339Nano),
		"id":   current.ID,
		"name": name,
	}
	if previous != nil {
		document["preread"] = previous.Read.UTC().Format(time.RFC3339Nano)
	}
	if current.CPU != nil {
		document["cpu_stats"] = dockerCPUStats(current.CPU)
		if previous != nil && previous.CPU != nil {
			document["precpu_stats"] = dockerCPUStats(previous.CPU)
		}
	}
	if current.Memory != nil {
		memory := map[string]any{
			"stats": current.Memory.Stats,
		}
		if current.Memory.Usage != nil {
			memory["usage"] = *current.Memory.Usage
		}
		if current.Memory.MaxUsage != nil {
			memory["max_usage"] = *current.Memory.MaxUsage
		}
		if current.Memory.Limit != nil {
			memory["limit"] = *current.Memory.Limit
		}
		document["memory_stats"] = memory
	}
	if current.PIDs != nil {
		document["pids_stats"] = map[string]uint64{
			"current": current.PIDs.Current,
			"limit":   current.PIDs.Limit,
		}
		document["num_procs"] = current.PIDs.Current
	}
	if len(current.Networks) > 0 {
		networks := make(map[string]any, len(current.Networks))
		for name, network := range current.Networks {
			networks[name] = map[string]uint64{
				"rx_bytes":   network.RxBytes,
				"rx_packets": network.RxPackets,
				"rx_errors":  network.RxErrors,
				"rx_dropped": network.RxDropped,
				"tx_bytes":   network.TxBytes,
				"tx_packets": network.TxPackets,
				"tx_errors":  network.TxErrors,
				"tx_dropped": network.TxDropped,
			}
		}
		document["networks"] = networks
	}
	if len(current.BlockIO) > 0 {
		entries := make([]map[string]any, 0, len(current.BlockIO))
		for _, entry := range current.BlockIO {
			entries = append(entries, map[string]any{
				"major": entry.Major,
				"minor": entry.Minor,
				"op":    entry.Op,
				"value": entry.Value,
			})
		}
		document["blkio_stats"] = map[string]any{
			"io_service_bytes_recursive": entries,
		}
	}
	return document
}

func dockerCPUStats(cpu *ContainerMetricCPU) map[string]any {
	usage := map[string]any{
		"total_usage":         cpu.TotalUsage,
		"usage_in_usermode":   cpu.UserUsage,
		"usage_in_kernelmode": cpu.SystemUsage,
	}
	document := map[string]any{
		"cpu_usage": usage,
		"throttling_data": map[string]uint64{
			"periods":           cpu.Throttling.Periods,
			"throttled_periods": cpu.Throttling.ThrottledPeriods,
			"throttled_time":    cpu.Throttling.ThrottledTime,
		},
	}
	if len(cpu.PerCPUUsage) > 0 {
		usage["percpu_usage"] = cpu.PerCPUUsage
	}
	if cpu.SystemCPUUsage != nil {
		document["system_cpu_usage"] = *cpu.SystemCPUUsage
	}
	if cpu.OnlineCPUs != 0 {
		document["online_cpus"] = cpu.OnlineCPUs
	}
	return document
}
