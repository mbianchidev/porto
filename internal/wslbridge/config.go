package wslbridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const (
	DistributionHeader = "X-Porto-WSL-Distribution"
	OwnerHeader        = "X-Porto-WSL-Owner"
)

type Config struct {
	Distribution      string `json:"distribution"`
	Owner             string `json:"owner"`
	WindowsExecutable string `json:"windowsExecutable"`
	WindowsHome       string `json:"windowsHome"`
	Socket            string `json:"socket"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var configuration Config
	if err := json.Unmarshal(data, &configuration); err != nil {
		return Config{}, err
	}
	if configuration.Owner == "" || configuration.Distribution == "" ||
		!filepath.IsAbs(configuration.WindowsExecutable) || !filepath.IsAbs(configuration.Socket) ||
		strings.ContainsAny(configuration.Distribution, "\x00\r\n") {
		return Config{}, errors.New("invalid owned WSL bridge configuration; repair with 'porto wsl enable'")
	}
	return configuration, nil
}

func (c Config) WindowsArguments(action string) []string {
	return []string{"wsl", action, "--distribution", c.Distribution, "--owner", c.Owner, "--home", c.WindowsHome}
}
