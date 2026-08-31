package process

import (
	"runtime"
	"strings"
)

func WithEnvironment(environment []string, values ...string) []string {
	keys := make(map[string]struct{}, len(values))
	for _, value := range values {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			keys[environmentKey(key)] = struct{}{}
		}
	}
	result := make([]string, 0, len(environment)+len(values))
	for _, value := range environment {
		key, _, ok := strings.Cut(value, "=")
		if ok {
			if _, replaced := keys[environmentKey(key)]; replaced {
				continue
			}
		}
		result = append(result, value)
	}
	return append(result, values...)
}

func environmentKey(value string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(value)
	}
	return value
}
