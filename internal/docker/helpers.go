package docker

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/config"
)

func decodeLines[T any](output []byte, convert func(map[string]string) T) ([]T, error) {
	items := make([]T, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("decode container runtime output line %q: %w", line, err)
		}
		item := make(map[string]string, len(raw))
		for key, value := range raw {
			switch typed := value.(type) {
			case string:
				item[key] = typed
			case float64:
				item[key] = strconv.FormatFloat(typed, 'f', -1, 64)
			case bool:
				item[key] = strconv.FormatBool(typed)
			default:
				encoded, _ := json.Marshal(typed)
				item[key] = string(encoded)
			}
		}
		items = append(items, convert(item))
	}
	return items, scanner.Err()
}

func first(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if values[key] != "" {
			return values[key]
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstVersionLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func installedStatus(backend commandBackend, versionOutput []byte) Status {
	serverVersion := firstVersionLine(string(versionOutput))
	if serverVersion == "" {
		serverVersion = config.Version
	}
	return Status{
		Available:     true,
		Context:       "porto",
		ClientVersion: config.Version,
		ServerVersion: serverVersion,
		Backend:       backend.description,
	}
}

func validateObjectID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("Docker object identifier is required")
	}
	if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid Docker object identifier %q", value)
	}
	return nil
}

func parseLabels(labels string) map[string]string {
	values := map[string]string{}
	for _, label := range strings.Split(labels, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(label), "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	return values
}

func appendStringFlag(args []string, name, value string) []string {
	if strings.TrimSpace(value) != "" {
		return append(args, name, value)
	}
	return args
}

func dockerEndpoint(socketPath string) string {
	if strings.HasPrefix(socketPath, `\\.\pipe\`) {
		return "npipe:////./pipe/" + strings.TrimPrefix(socketPath, `\\.\pipe\`)
	}
	if socketPath == "" {
		return ""
	}
	return "unix://" + socketPath
}

func EndpointURL(socketPath string) string {
	return dockerEndpoint(socketPath)
}

func randomResourceName() (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate Docker resource name: %w", err)
	}
	return "porto-" + hex.EncodeToString(random), nil
}

func normalizeNerdctlReference(reference string) string {
	name, digest, ok := strings.Cut(reference, "@")
	if !ok {
		return reference
	}
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		if colon := strings.LastIndexByte(name[slash+1:], ':'); colon >= 0 {
			name = name[:slash+1+colon]
		}
	} else if colon := strings.LastIndexByte(name, ':'); colon >= 0 {
		name = name[:colon]
	}
	return name + "@" + digest
}

func isImageDigest(value string) bool {
	algorithm, encoded, ok := strings.Cut(value, ":")
	if !ok || algorithm == "" || len(encoded) < 32 {
		return false
	}
	for _, character := range encoded {
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') &&
			(character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}
