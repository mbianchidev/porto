package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type kindImageCopy struct {
	Node       string   `json:"node"`
	Platform   string   `json:"platform"`
	References []string `json:"references"`
	File       string   `json:"file"`
	SHA256     string   `json:"sha256"`
}

func collectUpgradeImages(value any, images map[string]struct{}) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "containers" || key == "initContainers" {
				containers, _ := child.([]any)
				for _, raw := range containers {
					container, _ := raw.(map[string]any)
					image, _ := container["image"].(string)
					if parsed, err := reference.ParseNormalizedNamed(image); err == nil {
						images[reference.TagNameOnly(parsed).String()] = struct{}{}
					}
				}
			}
			collectUpgradeImages(child, images)
		}
	case []any:
		for _, child := range value {
			collectUpgradeImages(child, images)
		}
	}
}

func (p *ClusterProvisioner) findKindUpgradeImages(ctx context.Context, spec ClusterRequest, required map[string]struct{}) ([]kindImageCopy, error) {
	var copies []kindImageCopy
	if len(required) == 0 {
		return copies, nil
	}
	for _, node := range kindNodeNames(spec) {
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "ctr", "--namespace", "k8s.io", "images", "ls", "-q"}, Env: p.portoDockerEnv()})
		if err != nil {
			return nil, runtimes.CommandError("inspect application images on "+node, output, err)
		}
		imageCopy := kindImageCopy{Node: node}
		for _, image := range strings.Fields(string(output)) {
			if _, wanted := required[image]; wanted {
				imageCopy.References = append(imageCopy.References, image)
				delete(required, image)
			}
		}
		if len(imageCopy.References) == 0 {
			continue
		}
		sort.Strings(imageCopy.References)
		output, err = p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "uname", "-m"}, Env: p.portoDockerEnv()})
		if err != nil {
			return nil, runtimes.CommandError("inspect image platform", output, err)
		}
		switch strings.TrimSpace(string(output)) {
		case "aarch64", "arm64":
			imageCopy.Platform = "linux/arm64"
		case "x86_64":
			imageCopy.Platform = "linux/amd64"
		default:
			return nil, fmt.Errorf("unsupported kind node architecture %q", strings.TrimSpace(string(output)))
		}
		copies = append(copies, imageCopy)
	}
	if len(required) > 0 {
		return nil, errors.New("some application images are unavailable on every kind node; make all workloads runnable before upgrading")
	}
	return copies, nil
}

func validateUpgradePodVolumes(object map[string]any) error {
	switch object["kind"] {
	case "Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicationController":
	default:
		return nil
	}
	var visit func(any) error
	visit = func(value any) error {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "volumes" {
					volumes, _ := child.([]any)
					for _, raw := range volumes {
						volume, _ := raw.(map[string]any)
						for source := range volume {
							switch source {
							case "name", "persistentVolumeClaim", "configMap", "secret", "projected", "downwardAPI", "emptyDir":
							default:
								return fmt.Errorf("application volume %v uses unsupported %s storage; migrate it to a local-path PVC before kind upgrade", volume["name"], source)
							}
						}
					}
				}
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(object["spec"])
}
