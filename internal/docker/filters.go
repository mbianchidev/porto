package docker

import (
	"encoding/json"
	"fmt"
	"strings"
)

type dockerFilters map[string]map[string]bool

func parseDockerFilters(raw string) (dockerFilters, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, fmt.Errorf("invalid Docker filters: %w", err)
	}
	filters := make(dockerFilters, len(fields))
	for key, field := range fields {
		var values map[string]bool
		if err := json.Unmarshal(field, &values); err != nil {
			var entries []string
			if err := json.Unmarshal(field, &entries); err != nil {
				return nil, fmt.Errorf("invalid Docker filter %s: %w", key, err)
			}
			values = make(map[string]bool, len(entries))
			for _, entry := range entries {
				values[entry] = true
			}
		}
		filters[key] = values
	}
	return filters, nil
}

func (filters dockerFilters) matchesName(name string) bool {
	values := filters["name"]
	if len(values) == 0 {
		return true
	}
	name = strings.TrimPrefix(name, "/")
	for value, enabled := range values {
		if enabled && strings.Contains(name, strings.Trim(value, "^$")) {
			return true
		}
	}
	return false
}

func (filters dockerFilters) matchesValue(key, value string) bool {
	values := filters[key]
	if len(values) == 0 {
		return true
	}
	for candidate, enabled := range values {
		if enabled && candidate == value {
			return true
		}
	}
	return false
}

func (filters dockerFilters) matchesID(id string) bool {
	values := filters["id"]
	if len(values) == 0 {
		return true
	}
	for candidate, enabled := range values {
		if enabled && strings.HasPrefix(id, candidate) {
			return true
		}
	}
	return false
}

func (filters dockerFilters) matchesLabels(labels map[string]string) bool {
	values := filters["label"]
	for value, enabled := range values {
		if !enabled {
			continue
		}
		key, expected, hasValue := strings.Cut(value, "=")
		actual, exists := labels[key]
		if !exists || (hasValue && actual != expected) {
			return false
		}
	}
	return true
}
