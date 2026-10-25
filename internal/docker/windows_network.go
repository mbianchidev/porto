package docker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func (m *Manager) ownedWindowsNetworkName(name string) (string, error) {
	if err := validateObjectID(name); err != nil {
		return "", err
	}
	switch strings.ToLower(name) {
	case "default", "nat":
		return m.windows.installation.DefaultNetwork(), nil
	case "none":
		return "none", nil
	case "host", "bridge":
		return "", errors.New("invalid native Windows network: select a private NAT network or none")
	}
	prefix := "porto-" + windowscontainers.OwnerKey(m.windows.installation.OwnerSID) + "-"
	if strings.HasPrefix(name, "porto-") && !strings.HasPrefix(name, prefix) {
		return "", windowscontainers.ErrOwnership
	}
	if !strings.HasPrefix(name, prefix) {
		name = prefix + name
	}
	return name, windowscontainers.ValidateNetworkName(m.windows.installation, name)
}

func (m *Manager) windowsNetworkIdentifier(ctx context.Context, name string) (string, error) {
	isID := len(name) >= 12 && len(name) <= 64 && strings.Trim(name, "0123456789abcdef") == ""
	if !isID {
		return m.ownedWindowsNetworkName(name)
	}
	document, err := m.inspect(ctx, "network", name)
	if err != nil {
		return "", err
	}
	var network struct{ Name string }
	if err := json.Unmarshal(document, &network); err != nil {
		return "", err
	}
	if err := windowscontainers.ValidateNetworkName(m.windows.installation, network.Name); err != nil {
		return "", err
	}
	return network.Name, nil
}
