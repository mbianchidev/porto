//go:build windows

package networking

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func systemProxy() (Config, string, error) {
	if configuration, configured := environmentProxy(); configured {
		return configuration, "environment", nil
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return Config{Mode: "system"}, "Windows static system proxy", nil
	}
	if err != nil {
		return Config{}, "", fmt.Errorf("read Windows proxy settings: %w", err)
	}
	defer key.Close()
	pac, _, err := key.GetStringValue("AutoConfigURL")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return Config{}, "", err
	}
	autoDetect, _, err := key.GetIntegerValue("AutoDetect")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return Config{}, "", err
	}
	if pac != "" || autoDetect != 0 {
		return Config{}, "", errorsPAC()
	}
	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return Config{}, "", err
	}
	configuration := Config{Mode: "system"}
	if enabled == 0 {
		return configuration, "Windows static system proxy", nil
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return Config{}, "", fmt.Errorf("read enabled Windows proxy server: %w", err)
	}
	bypass, _, err := key.GetStringValue("ProxyOverride")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return Config{}, "", err
	}
	configuration.NoProxy = strings.ReplaceAll(bypass, ";", ",")
	for _, entry := range strings.Split(server, ";") {
		protocol, address, scoped := strings.Cut(entry, "=")
		if !scoped {
			protocol, address = "", entry
		}
		address = strings.TrimSpace(address)
		if address == "" {
			return Config{}, "", errors.New("Windows proxy is enabled without an address")
		}
		scheme := "http"
		if protocol == "socks" {
			scheme = "socks5"
		}
		if !strings.Contains(address, "://") {
			address = scheme + "://" + address
		}
		if _, err := parseProxy(address); err != nil {
			return Config{}, "", fmt.Errorf("invalid Windows proxy setting: %w", err)
		}
		switch protocol {
		case "":
			configuration.HTTPProxy, configuration.HTTPSProxy = address, address
		case "http":
			configuration.HTTPProxy = address
		case "https":
			configuration.HTTPSProxy = address
		case "socks":
			configuration.SOCKSProxy = address
		default:
			return Config{}, "", fmt.Errorf("Windows proxy protocol %q is unsupported; configure Porto's proxy settings explicitly", protocol)
		}
	}
	return configuration, "Windows static system proxy", nil
}
