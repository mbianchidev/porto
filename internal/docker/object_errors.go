package docker

import (
	"errors"
	"strings"
)

func missingDockerObject(err error, kind string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) {
		return true
	}
	message := strings.ToLower(err.Error())
	if kind == "volume" && strings.Contains(message, "volume-store error") &&
		strings.Contains(message, "/volume.json") && strings.Contains(message, "does not exist") {
		return true
	}
	return strings.Contains(message, "no such "+kind) ||
		strings.Contains(message, "no "+kind+" found matching:") ||
		strings.Contains(message, kind+" not found") ||
		strings.Contains(message, kind+" does not exist")
}
