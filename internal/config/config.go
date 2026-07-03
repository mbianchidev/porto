package config

import (
	"errors"
	"os"
	"path/filepath"
)

const (
	AppName          = "porto"
	DaemonAddr       = "127.0.0.1:37623"
	RouterAddr       = "127.0.0.1:37680"
	BasePort         = 41000
	DefaultScanDepth = 3
)

func Dir() (string, error) {
	if custom := os.Getenv("PORTO_HOME"); custom != "" {
		return custom, os.MkdirAll(custom, 0o755)
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil || home == "" {
			return "", errors.New("cannot determine config directory")
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, AppName)
	return dir, os.MkdirAll(dir, 0o755)
}

func DBPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "porto.db"), nil
}
