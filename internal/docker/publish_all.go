package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/network"
)

func (m *Manager) publishAllContainerPorts(ctx context.Context, request *CreateContainerRequest) error {
	reference := normalizeNerdctlReference(request.Image)
	document, err := m.inspect(ctx, "image", reference)
	if err != nil {
		if !missingDockerObject(err, "image") {
			return err
		}
		if err := m.PullImage(ctx, reference, request.Platform); err != nil {
			return err
		}
		document, err = m.inspect(ctx, "image", reference)
		if err != nil {
			return err
		}
	}
	var image struct {
		Config struct{ ExposedPorts map[string]struct{} }
	}
	if err := json.Unmarshal(document, &image); err != nil {
		return fmt.Errorf("decode image ports for publish-all: %w", err)
	}
	ports := make(map[string]bool)
	add := func(value string) error {
		port, err := network.ParsePort(value)
		if err != nil {
			return fmt.Errorf("invalid image exposed port %q: %w", value, err)
		}
		ports[port.String()] = true
		return nil
	}
	for value := range image.Config.ExposedPorts {
		if err := add(value); err != nil {
			return err
		}
	}
	for _, value := range request.Expose {
		if err := add(value); err != nil {
			return err
		}
	}
	for _, published := range request.Publish {
		target := published[strings.LastIndex(published, ":")+1:]
		port, err := network.ParsePort(target)
		if err != nil {
			return err
		}
		delete(ports, port.String())
	}
	ordered := make([]string, 0, len(ports))
	for port := range ports {
		ordered = append(ordered, port)
	}
	slices.Sort(ordered)
	for _, value := range ordered {
		port, err := network.ParsePort(value)
		if err != nil {
			return err
		}
		hostPort, err := availablePublishedPort(string(port.Proto()))
		if err != nil {
			return fmt.Errorf("allocate published container port: %w", err)
		}
		request.Publish = append(request.Publish, strconv.Itoa(hostPort)+":"+value)
	}
	return nil
}

func availablePublishedPort(protocol string) (int, error) {
	switch protocol {
	case "tcp":
		listener, err := net.Listen("tcp", ":0")
		if err != nil {
			return 0, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		return port, listener.Close()
	case "udp":
		listener, err := net.ListenPacket("udp", ":0")
		if err != nil {
			return 0, err
		}
		port := listener.LocalAddr().(*net.UDPAddr).Port
		return port, listener.Close()
	default:
		return 0, fmt.Errorf("%w: automatic %s port allocation", ErrUnsupported, protocol)
	}
}
