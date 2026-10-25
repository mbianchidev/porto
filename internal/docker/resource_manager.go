package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func (m *Manager) CreateNetwork(ctx context.Context, request CreateNetworkRequest) (string, error) {
	if err := validateObjectID(request.Name); err != nil {
		return "", err
	}
	if m.windows != nil {
		name, err := m.ownedWindowsNetworkName(request.Name)
		if err != nil {
			return "", err
		}
		request.Name = name
		if err := windowscontainers.ValidateNetworkName(m.windows.installation, request.Name); err != nil {
			return "", err
		}
		if (request.Driver != "" && request.Driver != "nat") || request.Internal || request.EnableIPv6 ||
			len(request.Subnets) > 1 || len(request.Gateways)+len(request.Options) != 0 {
			return "", fmt.Errorf("%w: native Windows networks require the nat driver, one explicit IPv4 subnet and default gateway/options", ErrUnsupported)
		}
		if len(request.Subnets) == 0 {
			sum := sha256.Sum256([]byte(request.Name))
			request.Subnets = []string{fmt.Sprintf("10.254.%d.0/24", sum[0])}
		}
		request.Driver = "nat"
		if _, err := m.windows.ensureNetwork(m.windows.installation, request.Name, request.Subnets[0]); err != nil {
			return "", err
		}
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
	ctx, release, err := m.dataReadGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := validateObjectID(name); err != nil {
		return err
	}
	if m.windows != nil {
		resolved, err := m.windowsNetworkIdentifier(ctx, name)
		if err != nil {
			return err
		}
		name = resolved
		if err := windowscontainers.ValidateNetworkName(m.windows.installation, name); err != nil {
			return err
		}
		if name == m.windows.installation.DefaultNetwork() {
			return fmt.Errorf("%w: the default native NAT network is preserved with engine data", ErrConflict)
		}
	}
	_, err = m.run(ctx, "remove Porto network", "network", "rm", name)
	if err == nil && m.windows != nil {
		return m.windows.removeNetwork(m.windows.installation, name)
	}
	return err
}

func (m *Manager) ConnectNetwork(ctx context.Context, network, container string, aliases []string) error {
	if m.windows != nil {
		return fmt.Errorf("%w: native Windows live network connect; choose the owned NAT network at container creation", ErrUnsupported)
	}
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
	if handled, err := m.withNetworkOperations(ctx, func(operations networkOperations) error {
		return operations.Connect(ctx, network, container, aliases)
	}); handled {
		if err == nil {
			m.invalidateContainerInventory()
		}
		return err
	}
	args = append(args, network, container)
	_, err := m.run(ctx, "connect Porto container network", args...)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}

func (m *Manager) DisconnectNetwork(ctx context.Context, network, container string, force bool) error {
	if m.windows != nil {
		return fmt.Errorf("%w: native Windows live network disconnect; stop/remove only the selected container explicitly", ErrUnsupported)
	}
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
	if handled, err := m.withNetworkOperations(ctx, func(operations networkOperations) error {
		return operations.Disconnect(ctx, network, container, force)
	}); handled {
		if err == nil {
			m.invalidateContainerInventory()
		}
		return err
	}
	args = append(args, network, container)
	_, err := m.run(ctx, "disconnect Porto container network", args...)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}

func (m *Manager) CreateVolume(ctx context.Context, name, driver string, labels map[string]string) (Volume, error) {
	if driver != "" && driver != "local" {
		return Volume{}, fmt.Errorf("%w: volume driver %q", ErrUnsupported, driver)
	}
	ctx, release, err := m.dataReadGate(ctx)
	if err != nil {
		return Volume{}, err
	}
	defer release()
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
	return m.removeVolume(ctx, name, force, nil)
}

func (m *Manager) removeVolume(ctx context.Context, name string, force bool, document json.RawMessage) error {
	ctx, release, err := m.dataReadGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := validateObjectID(name); err != nil {
		return err
	}
	if m.nativeGuard != nil {
		if err := m.nativeGuard("volume", name); err != nil {
			return err
		}
	}
	// Owned cleanup can reuse its fresh inspection under the same data-operation gate.
	if document == nil {
		document, err = m.InspectVolume(ctx, name)
		if err != nil {
			return err
		}
	}
	var volume struct {
		Name       string
		Mountpoint string
	}
	if err := json.Unmarshal(document, &volume); err != nil {
		return err
	}
	if volume.Name != name {
		return fmt.Errorf("%w: volume identity changed during removal", ErrConflict)
	}
	containers, err := m.storageContainers(ctx)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if containerUsesVolume(container, name, volume.Mountpoint) {
			return fmt.Errorf("%w: volume %s is referenced by container %s; remove or detach its mount first", ErrConflict, name, container.Name)
		}
	}
	args := []string{"volume", "rm"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, name)
	_, err = m.run(ctx, "remove Porto volume", args...)
	return err
}
