package windowscontainers

import (
	"errors"
	"fmt"
	"strings"
)

func windowsFeatureEnabled(output []byte) (bool, error) {
	state := ""
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "State" {
			continue
		}
		if state != "" {
			return false, errors.New("native Windows feature inspection returned ambiguous state")
		}
		state = strings.TrimSpace(value)
	}
	switch state {
	case "Enabled":
		return true, nil
	case "Disabled", "Disabled with Payload Removed":
		return false, nil
	case "Enable Pending", "Disable Pending":
		return false, errors.New("Windows feature state is pending; complete the requested reboot before native runtime setup")
	default:
		return false, fmt.Errorf("native Windows feature inspection did not return a supported English state: %q", state)
	}
}
