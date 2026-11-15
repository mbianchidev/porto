package docker

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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

// unsupportedKey returns the first filter Porto cannot evaluate. Ignoring such
// a filter would silently widen a listing that scripts may feed into removal.
func (filters dockerFilters) unsupportedKey(supported ...string) string {
	for _, key := range slices.Sorted(maps.Keys(filters)) {
		if !slices.Contains(supported, key) {
			return key
		}
	}
	return ""
}

// boolean reads a single-valued Docker boolean filter such as dangling.
func (filters dockerFilters) boolean(key string) (value, set bool, err error) {
	for candidate, enabled := range filters[key] {
		if !enabled {
			continue
		}
		var parsed bool
		switch strings.ToLower(strings.TrimSpace(candidate)) {
		case "true", "1":
			parsed = true
		case "false", "0":
		default:
			return false, false, fmt.Errorf("invalid filter '%s=%s'", key, candidate)
		}
		if set && parsed != value {
			return false, false, fmt.Errorf("invalid filter '%s': conflicting values", key)
		}
		value, set = parsed, true
	}
	return value, set, nil
}
