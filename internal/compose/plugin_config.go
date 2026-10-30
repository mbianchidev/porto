package compose

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/runtimes"
)

func isolatedPluginConfig(work string) (string, error) {
	directories := []string{}
	if binary, err := runtimes.LookPath("docker-compose"); err == nil {
		directories = append(directories, filepath.Dir(binary))
	}
	if configured := os.Getenv("DOCKER_CONFIG"); configured != "" {
		directories = append(directories, filepath.Join(configured, "cli-plugins"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, filepath.Join(home, ".docker", "cli-plugins"))
	}
	data, err := json.Marshal(struct {
		Directories []string `json:"cliPluginsExtraDirs"`
	}{directories})
	if err != nil {
		return "", err
	}
	root := filepath.Join(work, "docker-config")
	if err := config.WriteFileAtomic(filepath.Join(root, "config.json"), data, 0o600); err != nil {
		return "", err
	}
	return root, nil
}
