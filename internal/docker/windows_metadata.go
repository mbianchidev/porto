package docker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func persistedWindowsRequest(id string, labels map[string]string) (CreateContainerRequest, error) {
	var request CreateContainerRequest
	if err := json.Unmarshal([]byte(labels[windowsRequestLabel]), &request); err != nil {
		return request, fmt.Errorf("decode native container %s persisted request: %w", id, err)
	}
	if strings.TrimSpace(request.Image) == "" || len(request.Networks) != 1 || request.Networks[0].Name == "" {
		return request, fmt.Errorf("native container %s persisted request is missing its image or single network", id)
	}
	if _, err := windowscontainers.ParsePublishedPorts(request.Publish); err != nil {
		return request, fmt.Errorf("native container %s persisted published ports: %w", id, err)
	}
	return request, nil
}

func persistedWindowsAttachment(id string, labels map[string]string) (windowscontainers.Attachment, error) {
	var attachment windowscontainers.Attachment
	value := labels[windowsNetworkLabel]
	if value == "" {
		return attachment, nil
	}
	if err := json.Unmarshal([]byte(value), &attachment); err != nil {
		return attachment, fmt.Errorf("decode native container %s persisted attachment: %w", id, err)
	}
	if attachment.Container != id || attachment.Network == "" {
		return attachment, fmt.Errorf("native container %s persisted attachment identity is incomplete or changed", id)
	}
	return attachment, nil
}

func persistedWindowsExitCode(id string, labels map[string]string) (uint32, error) {
	value := labels["io.porto.windows.exit-code"]
	if value == "" {
		if labels[windowsStateLabel] == "exited" {
			return 0, fmt.Errorf("native container %s persisted exited state has no exit code", id)
		}
		return 0, nil
	}
	code, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse native container %s persisted exit code: %w", id, err)
	}
	return uint32(code), nil
}
