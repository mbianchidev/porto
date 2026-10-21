package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

var upgradeVolumeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
var upgradeContainerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

type kindVolumeMount struct {
	Name         string
	Image        string
	RelativePath string
}

func (p *ClusterProvisioner) kindPersistentVolumeMount(ctx context.Context, node, volumePath string) (kindVolumeMount, error) {
	output, err := p.runner.Run(ctx, runtimes.Command{
		Name: "docker",
		Args: []string{"inspect", "--format", `{"image":{{json .Image}},"mounts":{{json .Mounts}}}`, node},
		Env:  p.portoDockerEnv(),
	})
	if err != nil {
		return kindVolumeMount{}, runtimes.CommandError("inspect persistent node mounts", output, err)
	}
	var info struct {
		Image  string `json:"image"`
		Mounts []struct {
			Type        string `json:"type"`
			Name        string `json:"name"`
			Destination string `json:"destination"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return kindVolumeMount{}, fmt.Errorf("decode persistent node mounts: %w", err)
	}
	var result kindVolumeMount
	longest := 0
	for _, mount := range info.Mounts {
		prefix := path.Clean(mount.Destination) + "/"
		if mount.Type == "volume" && len(prefix) > longest && strings.HasPrefix(volumePath, prefix) &&
			upgradeVolumeNamePattern.MatchString(mount.Name) {
			result = kindVolumeMount{Name: mount.Name, Image: info.Image, RelativePath: strings.TrimPrefix(volumePath, prefix)}
			longest = len(prefix)
		}
	}
	if result.Name == "" || result.Image == "" || strings.HasPrefix(result.Image, "-") || strings.ContainsAny(result.Image, "\x00\r\n ") {
		return kindVolumeMount{}, errors.New("kind persistent data is not backed by an identifiable node volume and cached node image; upgrade refused")
	}
	return result, nil
}

func (p *ClusterProvisioner) createKindVolumeHelper(ctx context.Context, mount kindVolumeMount, restore bool) (string, error) {
	volume := "type=volume,source=" + mount.Name + ",target=/porto-volume"
	args := []string{"create", "--network", "none", "--user", "0", "--label", "porto.kubernetes.upgrade-helper=true"}
	if !restore {
		volume += ",readonly"
	}
	args = append(args, "--mount", volume, "--entrypoint", "/bin/sleep", mount.Image, "3600")
	output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: args, Env: p.portoDockerEnv()})
	if err != nil {
		return "", runtimes.CommandError("create private persistent-volume copy helper", output, err)
	}
	id := strings.TrimSpace(string(output))
	if !upgradeContainerIDPattern.MatchString(id) {
		return "", errors.New("persistent-volume helper returned no valid newly created container identity")
	}
	output, err = p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"start", id}, Env: p.portoDockerEnv()})
	if err != nil {
		return "", errors.Join(runtimes.CommandError("start private persistent-volume copy helper", output, err), p.removeKindVolumeHelper(id))
	}
	return id, nil
}

func (p *ClusterProvisioner) removeKindVolumeHelper(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output, err := p.runner.Run(ctx, runtimes.Command{
		Name: "docker", Args: []string{"rm", "--force", id}, Env: p.portoDockerEnv(),
	})
	if err != nil {
		return runtimes.CommandError("remove exact persistent-volume helper without deleting its mounted volume", output, err)
	}
	return nil
}
