//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	gocni "github.com/containerd/go-cni"
)

var cniPluginDirectories = []string{
	"/opt/cni/bin",
	"/usr/local/libexec/cni",
	"/usr/libexec/cni",
	"/usr/lib/cni",
}

func probeRuntime() runtimeProbe {
	probe := runtimeProbe{}
	configDirectory := cniConfigDirectory()
	if _, err := os.Stat(configDirectory); err != nil {
		probe.CNIReason = fmt.Sprintf("CNI configuration directory %s: %v", configDirectory, err)
	} else {
		for _, directory := range cniPluginDirectories {
			if info, err := os.Stat(directory); err == nil && info.IsDir() {
				probe.CNI = true
				break
			}
		}
		if !probe.CNI {
			probe.CNIReason = "no CNI plugin directory is available"
		}
	}
	if _, err := exec.LookPath("criu"); err == nil {
		probe.CRIU = true
	} else {
		probe.CRIUReason = "criu is not installed"
	}
	return probe
}

func connectCNI(ctx context.Context, request cniRequest) (any, error) {
	plugin, labels, err := loadCNI(request)
	if err != nil {
		return nil, err
	}
	result, err := plugin.SetupSerially(ctx, request.Container, request.NetNS, gocni.WithLabels(labels))
	if err != nil {
		return nil, fmt.Errorf("connect CNI network %q: %w", request.Network, err)
	}
	endpoint := cniEndpointResult{}
	for name, config := range result.Interfaces {
		if config == nil || (endpoint.Interface != "" && config.Sandbox == "") {
			continue
		}
		endpoint.Interface = name
		endpoint.MAC = config.Mac
		endpoint.Addresses = endpoint.Addresses[:0]
		endpoint.Gateways = endpoint.Gateways[:0]
		for _, ip := range config.IPConfigs {
			if ip == nil {
				continue
			}
			if ip.IP != nil {
				endpoint.Addresses = append(endpoint.Addresses, ip.IP.String())
			}
			if ip.Gateway != nil {
				endpoint.Gateways = append(endpoint.Gateways, ip.Gateway.String())
			}
		}
	}
	return endpoint, nil
}

func disconnectCNI(ctx context.Context, request cniRequest) error {
	plugin, labels, err := loadCNI(request)
	if err != nil {
		return err
	}
	if err := plugin.Remove(ctx, request.Container, request.NetNS, gocni.WithLabels(labels)); err != nil &&
		!gocni.IsNotFound(err) {
		return fmt.Errorf("disconnect CNI network %q: %w", request.Network, err)
	}
	return nil
}

func checkCNI(ctx context.Context, request cniRequest) error {
	plugin, labels, err := loadCNI(request)
	if err != nil {
		return err
	}
	if err := plugin.Check(ctx, request.Container, request.NetNS, gocni.WithLabels(labels)); err != nil {
		message := strings.ToLower(err.Error())
		if strings.Contains(message, "check is not supported") ||
			strings.Contains(message, "does not support check") {
			return errCNICheckUnsupported
		}
		return fmt.Errorf("check CNI network %q: %w", request.Network, err)
	}
	return nil
}

func loadCNI(request cniRequest) (gocni.CNI, map[string]string, error) {
	configPath, list, err := findCNIConfig(cniConfigDirectory(), request.Network)
	if err != nil {
		return nil, nil, err
	}
	pluginDirectories := make([]string, 0, len(cniPluginDirectories))
	for _, directory := range cniPluginDirectories {
		if info, statErr := os.Stat(directory); statErr == nil && info.IsDir() {
			pluginDirectories = append(pluginDirectories, directory)
		}
	}
	if len(pluginDirectories) == 0 {
		return nil, nil, errors.New("no CNI plugin directory is available")
	}
	plugin, err := gocni.New(
		gocni.WithMinNetworkCount(1),
		gocni.WithPluginConfDir(filepath.Dir(configPath)),
		gocni.WithPluginDir(pluginDirectories),
		gocni.WithInterfacePrefix(request.InterfacePrefix),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("initialize CNI: %w", err)
	}
	loadOption := gocni.WithConfFile(configPath)
	if list {
		loadOption = gocni.WithConfListFile(configPath)
	}
	if err := plugin.Load(loadOption); err != nil {
		return nil, nil, fmt.Errorf("load CNI network %q: %w", request.Network, err)
	}
	labels := map[string]string{
		"IgnoreUnknown":              "1",
		"K8S_POD_NAMESPACE":          "porto",
		"K8S_POD_INFRA_CONTAINER_ID": request.Container,
		"K8S_POD_NAME":               request.Container,
	}
	if len(request.Aliases) == 1 {
		labels["K8S_POD_NAME"] = request.Aliases[0]
	}
	return plugin, labels, nil
}

func cniConfigDirectory() string {
	if directory := strings.TrimSpace(os.Getenv("CNI_NET_DIR")); directory != "" {
		return directory
	}
	return gocni.DefaultNetDir
}

func findCNIConfig(directory, network string) (string, bool, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", false, fmt.Errorf("read CNI configuration directory %s: %w", directory, err)
	}
	slices.SortFunc(entries, func(left, right os.DirEntry) int {
		return strings.Compare(left.Name(), right.Name())
	})
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := filepath.Ext(entry.Name())
		if extension != ".conf" && extension != ".conflist" && extension != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		document, err := os.ReadFile(path)
		if err != nil {
			return "", false, fmt.Errorf("read CNI configuration %s: %w", path, err)
		}
		var metadata struct {
			Name    string            `json:"name"`
			Plugins []json.RawMessage `json:"plugins"`
		}
		if err := json.Unmarshal(document, &metadata); err != nil {
			continue
		}
		if metadata.Name == network {
			return path, len(metadata.Plugins) > 0 || extension == ".conflist", nil
		}
	}
	return "", false, fmt.Errorf("CNI network %q was not found in %s", network, directory)
}
