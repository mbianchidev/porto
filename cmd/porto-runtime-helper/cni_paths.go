package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const defaultCNIConfigDirectory = "/etc/cni/net.d"

func cniConfigDirectories() []string {
	if directory := strings.TrimSpace(os.Getenv("CNI_NET_DIR")); directory != "" {
		return []string{directory}
	}
	namespace := strings.TrimSpace(os.Getenv("CONTAINERD_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}
	if filepath.Base(namespace) != namespace || namespace == "." || namespace == ".." {
		namespace = ""
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
			configHome = filepath.Join(home, ".config")
		}
	}
	var directories []string
	appendBase := func(base string) {
		if base == "" {
			return
		}
		if namespace != "" {
			directories = append(directories, filepath.Join(base, namespace))
		}
		directories = append(directories, base)
	}
	if configHome != "" {
		appendBase(filepath.Join(configHome, "cni", "net.d"))
	}
	appendBase(defaultCNIConfigDirectory)
	return uniqueCNIPaths(directories)
}

func uniqueCNIPaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		cleaned := filepath.Clean(path)
		if cleaned == "." {
			continue
		}
		if _, exists := seen[cleaned]; exists {
			continue
		}
		seen[cleaned] = struct{}{}
		result = append(result, cleaned)
	}
	return result
}

func hasCNIConfig(directories []string) bool {
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch filepath.Ext(entry.Name()) {
			case ".conf", ".conflist", ".json":
				return true
			}
		}
	}
	return false
}

func findCNIConfig(directories []string, network string) (string, bool, error) {
	var available []string
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("read CNI configuration directory %s: %w", directory, err)
		}
		available = append(available, directory)
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
	}
	if len(available) == 0 {
		return "", false, fmt.Errorf(
			"no CNI configuration directory is available (checked %s)",
			strings.Join(directories, ", "),
		)
	}
	return "", false, fmt.Errorf(
		"CNI network %q was not found in %s",
		network,
		strings.Join(available, ", "),
	)
}
