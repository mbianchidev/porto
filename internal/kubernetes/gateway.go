package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

const (
	gatewayNamespace = "porto-system"
	gatewayName      = "porto"
)

var gatewayNameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

func (m *Manager) ApplyHTTPRoute(
	ctx context.Context,
	contextName string,
	namespace string,
	service string,
	port int32,
	hostname string,
) error {
	if err := validateResource(namespace, service); err != nil {
		return err
	}
	if port <= 0 || port > 65535 {
		return errors.New("Kubernetes service port must be between 1 and 65535")
	}
	hostname = strings.TrimSpace(strings.ToLower(hostname))
	if hostname == "" {
		return errors.New("Kubernetes route hostname is required")
	}
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata": map[string]any{
			"name":      HTTPRouteName(service, port),
			"namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "porto",
			},
		},
		"spec": map[string]any{
			"parentRefs": []map[string]any{{
				"name":      gatewayName,
				"namespace": gatewayNamespace,
			}},
			"hostnames": []string{
				hostname + "." + config.LocalhostDomain,
				hostname + "." + config.LocalDomain,
			},
			"rules": []map[string]any{{
				"backendRefs": []map[string]any{{
					"name": service,
					"port": port,
				}},
			}},
		},
	})
	if err != nil {
		return fmt.Errorf("encode Kubernetes HTTPRoute: %w", err)
	}
	if _, err := m.run(ctx, contextName, m.timeout, manifest, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply Kubernetes HTTPRoute for %s/%s:%d: %w", namespace, service, port, err)
	}
	return nil
}

func (m *Manager) DeleteHTTPRoute(
	ctx context.Context,
	contextName string,
	namespace string,
	service string,
	port int32,
) error {
	if err := validateResource(namespace, service); err != nil {
		return err
	}
	_, err := m.run(
		ctx,
		contextName,
		m.timeout,
		nil,
		"delete",
		"httproute",
		HTTPRouteName(service, port),
		"--namespace",
		namespace,
		"--ignore-not-found=true",
	)
	return err
}

func (m *Manager) GatewayServiceName(ctx context.Context, contextName string) (string, error) {
	output, err := m.run(
		ctx,
		contextName,
		30*time.Second,
		nil,
		"get",
		"services",
		"--namespace",
		"envoy-gateway-system",
		"--selector",
		"gateway.envoyproxy.io/owning-gateway-namespace="+gatewayNamespace+
			",gateway.envoyproxy.io/owning-gateway-name="+gatewayName,
		"-o",
		"jsonpath={.items[0].metadata.name}",
	)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(output))
	if name == "" {
		return "", errors.New("Envoy Gateway data-plane service is not ready")
	}
	return name, nil
}

func HTTPRouteName(service string, port int32) string {
	name := "porto-" + strings.ToLower(service) + "-" + strconv.Itoa(int(port))
	name = strings.Trim(gatewayNameCharacters.ReplaceAllString(name, "-"), "-")
	if len(name) <= 63 {
		return name
	}
	suffix := "-" + shortGatewayHash(name)
	return strings.TrimRight(name[:63-len(suffix)], "-") + suffix
}

func shortGatewayHash(value string) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	var hash uint32 = 2166136261
	for index := 0; index < len(value); index++ {
		hash ^= uint32(value[index])
		hash *= 16777619
	}
	buffer := [6]byte{}
	for index := len(buffer) - 1; index >= 0; index-- {
		buffer[index] = alphabet[hash%uint32(len(alphabet))]
		hash /= uint32(len(alphabet))
	}
	return string(buffer[:])
}
