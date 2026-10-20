package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type dockerAPIVersionKey struct{}

func dockerRequestVersion(r *http.Request) string {
	if version, ok := r.Context().Value(dockerAPIVersionKey{}).(string); ok {
		return version
	}
	return dockerAPIVersion
}

func dockerVersionBefore(version, minimum string) bool {
	left := strings.Split(version, ".")
	right := strings.Split(minimum, ".")
	leftMajor, _ := strconv.Atoi(left[0])
	leftMinor, _ := strconv.Atoi(left[1])
	rightMajor, _ := strconv.Atoi(right[0])
	rightMinor, _ := strconv.Atoi(right[1])
	return leftMajor < rightMajor || (leftMajor == rightMajor && leftMinor < rightMinor)
}

func dockerVersionedPath(path string) (string, string, error) {
	if !strings.HasPrefix(path, "/v") {
		return path, dockerAPIVersion, nil
	}
	prefix, remainder, found := strings.Cut(path[2:], "/")
	if !found || prefix == "" || prefix[0] < '0' || prefix[0] > '9' {
		return path, dockerAPIVersion, nil
	}
	parts := strings.Split(prefix, ".")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid API version %q", prefix)
	}
	for _, part := range parts {
		if part == "" {
			return "", "", fmt.Errorf("invalid API version %q", prefix)
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return "", "", fmt.Errorf("invalid API version %q", prefix)
			}
		}
		if _, err := strconv.Atoi(part); err != nil {
			return "", "", fmt.Errorf("invalid API version %q", prefix)
		}
	}
	if dockerVersionBefore(prefix, dockerMinAPIVersion) {
		return "", "", fmt.Errorf("client version %s is too old. Minimum supported API version is %s, please upgrade your client to a newer version", prefix, dockerMinAPIVersion)
	}
	if dockerVersionBefore(dockerAPIVersion, prefix) {
		return "", "", fmt.Errorf("client version %s is too new. Maximum supported API version is %s", prefix, dockerAPIVersion)
	}
	return "/" + remainder, prefix, nil
}

func dockerVersionedContext(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, dockerAPIVersionKey{}, version)
}

func dockerInspectForVersion(value json.RawMessage, version string, image bool) (json.RawMessage, error) {
	var document map[string]any
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, fmt.Errorf("decode Docker inspect response: %w", err)
	}
	if image {
		if !dockerVersionBefore(version, "1.44") {
			delete(document, "VirtualSize")
		}
		if !dockerVersionBefore(version, "1.45") {
			delete(document, "Container")
			delete(document, "ContainerConfig")
		}
	} else if config, ok := document["Config"].(map[string]any); ok {
		if labels, ok := config["Labels"].(map[string]any); ok {
			if labels[portoTTYLabel] == "true" {
				config["Tty"] = true
			}
			if labels[portoOpenStdinLabel] == "true" {
				config["OpenStdin"] = true
			}
			if labels["io.porto.container.stdin-once"] == "true" {
				config["StdinOnce"] = true
			}
			if encoded, ok := labels[portoExposedPortsLabel].(string); ok && encoded != "" {
				var ports []string
				if err := json.Unmarshal([]byte(encoded), &ports); err != nil {
					return nil, fmt.Errorf("decode exposed container ports: %w", err)
				}
				exposed, ok := config["ExposedPorts"].(map[string]any)
				if !ok {
					exposed = make(map[string]any)
				}
				for _, port := range ports {
					exposed[port] = map[string]any{}
				}
				config["ExposedPorts"] = exposed
			}
		}
	}
	if dockerVersionBefore(version, "1.44") {
		if config, ok := document["Config"].(map[string]any); ok {
			if health, ok := config["Healthcheck"].(map[string]any); ok {
				delete(health, "StartInterval")
			}
		}
	}
	return json.Marshal(document)
}
