package docker

import (
	"context"
	"fmt"
	"strings"
)

func (m *Manager) CreateNetwork(ctx context.Context, request CreateNetworkRequest) (string, error) {
	if err := validateObjectID(request.Name); err != nil {
		return "", err
	}
	args := []string{"network", "create"}
	args = appendStringFlag(args, "--driver", request.Driver)
	for _, subnet := range request.Subnets {
		args = appendStringFlag(args, "--subnet", subnet)
	}
	for _, gateway := range request.Gateways {
		args = appendStringFlag(args, "--gateway", gateway)
	}
	if request.Internal {
		args = append(args, "--internal")
	}
	if request.EnableIPv6 {
		args = append(args, "--ipv6")
	}
	for _, key := range sortedStringKeys(request.Options) {
		args = append(args, "--opt", key+"="+request.Options[key])
	}
	for key, value := range request.Labels {
		args = append(args, "--label", key+"="+value)
	}
	args = append(args, request.Name)
	output, err := m.run(ctx, "create Porto network", args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (m *Manager) RemoveNetwork(ctx context.Context, name string) error {
	if err := validateObjectID(name); err != nil {
		return err
	}
	_, err := m.run(ctx, "remove Porto network", "network", "rm", name)
	return err
}

func (m *Manager) ConnectNetwork(ctx context.Context, network, container string, aliases []string) error {
	if err := validateObjectID(network); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if err := validateObjectID(container); err != nil {
		return fmt.Errorf("container: %w", err)
	}
	args := []string{"network", "connect"}
	for _, alias := range aliases {
		if err := validateObjectID(alias); err != nil {
			return fmt.Errorf("network alias: %w", err)
		}
		args = append(args, "--alias", alias)
	}
	args = append(args, network, container)
	_, err := m.run(ctx, "connect Porto container network", args...)
	return err
}

func (m *Manager) DisconnectNetwork(ctx context.Context, network, container string, force bool) error {
	if err := validateObjectID(network); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	if err := validateObjectID(container); err != nil {
		return fmt.Errorf("container: %w", err)
	}
	args := []string{"network", "disconnect"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, network, container)
	_, err := m.run(ctx, "disconnect Porto container network", args...)
	return err
}

func (m *Manager) CreateVolume(ctx context.Context, name, driver string, labels map[string]string) (Volume, error) {
	if strings.TrimSpace(name) == "" {
		generated, err := randomResourceName()
		if err != nil {
			return Volume{}, err
		}
		name = generated
	}
	if err := validateObjectID(name); err != nil {
		return Volume{}, err
	}
	args := []string{"volume", "create"}
	args = appendStringFlag(args, "--driver", driver)
	for key, value := range labels {
		args = append(args, "--label", key+"="+value)
	}
	args = append(args, name)
	output, err := m.run(ctx, "create Porto volume", args...)
	if err != nil {
		return Volume{}, err
	}
	createdName := strings.TrimSpace(string(output))
	if createdName == "" {
		createdName = name
	}
	return Volume{Name: createdName, Driver: firstNonEmpty(driver, "local"), Scope: "local", Labels: labels}, nil
}

func (m *Manager) RemoveVolume(ctx context.Context, name string, force bool) error {
	if err := validateObjectID(name); err != nil {
		return err
	}
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, err := m.run(ctx, "remove Porto volume", args...)
	return err
}
