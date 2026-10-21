package docker

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type dockerDeviceRequest struct {
	Driver       string            `json:"Driver"`
	Count        int64             `json:"Count"`
	DeviceIDs    []string          `json:"DeviceIDs"`
	Capabilities [][]string        `json:"Capabilities"`
	Options      map[string]string `json:"Options"`
}

func dockerGPUArguments(requests []dockerDeviceRequest) ([]string, error) {
	result := make([]string, 0, len(requests))
	for _, request := range requests {
		if (request.Driver != "" && request.Driver != "nvidia") || len(request.Options) > 0 {
			return nil, fmt.Errorf("%w: GPU driver %q or custom device options", ErrUnsupported, request.Driver)
		}
		if request.Count < -1 || (request.Count != 0 && len(request.DeviceIDs) > 0) {
			return nil, errors.New("invalid GPU count or combined GPU count and device IDs")
		}
		if len(request.Capabilities) != 1 {
			return nil, fmt.Errorf("%w: GPU capability alternatives", ErrUnsupported)
		}
		gpu := false
		capabilities := make([]string, 0)
		for _, capability := range request.Capabilities[0] {
			switch capability {
			case "gpu":
				gpu = true
			case "compute", "compat32", "graphics", "utility", "video", "display":
				capabilities = append(capabilities, capability)
			default:
				return nil, fmt.Errorf("%w: GPU capability %q", ErrUnsupported, capability)
			}
		}
		if !gpu {
			return nil, fmt.Errorf("%w: device request is not a GPU request", ErrUnsupported)
		}
		fields := []string{}
		if len(request.DeviceIDs) > 0 {
			for _, id := range request.DeviceIDs {
				if id == "" || strings.ContainsAny(id, ",\r\n\x00") {
					return nil, errors.New("invalid GPU device ID")
				}
			}
			fields = append(fields, "device="+strings.Join(request.DeviceIDs, ","))
		} else if request.Count == -1 {
			fields = append(fields, "all")
		} else {
			count := request.Count
			if count == 0 {
				count = 1
			}
			fields = append(fields, "count="+strconv.FormatInt(count, 10))
		}
		if len(capabilities) > 0 {
			fields = append(fields, "capabilities="+strings.Join(capabilities, ","))
		}
		value, err := dockerCSV(fields)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}
