package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/mbianchidev/porto/internal/runtimes"
	digest "github.com/opencontainers/go-digest"
)

type kindImageCopy struct {
	Node            string   `json:"node"`
	Platform        string   `json:"platform"`
	References      []string `json:"references"`
	File            string   `json:"file"`
	SHA256          string   `json:"sha256"`
	LocalExport     bool     `json:"localExport,omitempty"`
	PinnedReference string   `json:"pinnedReference"`
	AllowPull       bool     `json:"allowPull"`
	Pull            bool     `json:"pull,omitempty"`
}

func collectUpgradeImages(value any, images map[string]bool) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "containers" || key == "initContainers" {
				containers, _ := child.([]any)
				for _, raw := range containers {
					container, _ := raw.(map[string]any)
					image, _ := container["image"].(string)
					if parsed, err := reference.ParseNormalizedNamed(image); err == nil {
						name := reference.TagNameOnly(parsed).String()
						images[name] = images[name] || container["imagePullPolicy"] == "Never"
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

func (p *ClusterProvisioner) findKindUpgradeImages(ctx context.Context, spec ClusterRequest, required map[string]bool) ([]kindImageCopy, error) {
	var copies []kindImageCopy
	if len(required) == 0 {
		return copies, nil
	}
	for _, node := range kindNodeNames(spec) {
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "ctr", "--namespace", "k8s.io", "images", "ls"}, Env: p.portoDockerEnv()})
		if err != nil {
			return nil, runtimes.CommandError("inspect application images on "+node, output, err)
		}
		var images []kindImageCopy
		for _, line := range strings.Split(string(output), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			image := fields[0]
			if neverPull, wanted := required[image]; wanted {
				imageDigest, err := digest.Parse(fields[2])
				if err != nil {
					return nil, fmt.Errorf("invalid cached image digest for %s: %w", image, err)
				}
				named, err := reference.ParseNormalizedNamed(image)
				if err != nil {
					return nil, err
				}
				pinned, err := reference.WithDigest(reference.TrimNamed(named), imageDigest)
				if err != nil {
					return nil, err
				}
				images = append(images, kindImageCopy{Node: node, References: []string{image}, PinnedReference: pinned.String(), AllowPull: !neverPull})
				delete(required, image)
			}
		}
		if len(images) == 0 {
			continue
		}
		sort.Slice(images, func(i, j int) bool { return images[i].References[0] < images[j].References[0] })
		output, err = p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "uname", "-m"}, Env: p.portoDockerEnv()})
		if err != nil {
			return nil, runtimes.CommandError("inspect image platform", output, err)
		}
		var platform string
		switch strings.TrimSpace(string(output)) {
		case "aarch64", "arm64":
			platform = "linux/arm64"
		case "x86_64":
			platform = "linux/amd64"
		default:
			return nil, fmt.Errorf("unsupported kind node architecture %q", strings.TrimSpace(string(output)))
		}
		output, err = p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "ctr", "--namespace", "k8s.io", "images", "export", "--help"}, Env: p.portoDockerEnv()})
		if err != nil {
			return nil, runtimes.CommandError("inspect image export capabilities", output, err)
		}
		for _, image := range images {
			image.Platform = platform
			image.LocalExport = strings.Contains(string(output), "--local")
			copies = append(copies, image)
		}
	}
	if len(required) > 0 {
		return nil, errors.New("some application images are unavailable on every kind node; make all workloads runnable before upgrading")
	}
	return copies, nil
}

func (p *ClusterProvisioner) restoreKindImages(ctx context.Context, nodes []string, filename string, image kindImageCopy) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, node := range nodes {
		output, err := p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: []string{"exec", node, "ctr", "--namespace", "k8s.io", "images", "import", "--help"}, Env: p.portoDockerEnv()})
		if err != nil {
			return runtimes.CommandError("inspect image import capabilities", output, err)
		}
		args := []string{"exec", "-i", node, "ctr", "--namespace", "k8s.io", "images", "import"}
		if strings.Contains(string(output), "--local") {
			args = append(args, "--local")
		}
		args = append(args, "--platform", image.Platform, "--snapshotter", "overlayfs", "--digests", "-")
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		output, err = p.runner.Run(ctx, runtimes.Command{Name: "docker", Args: args, Env: p.portoDockerEnv(), StdinReader: file})
		if err != nil {
			return runtimes.CommandError("restore application images on "+node, output, err)
		}
	}
	return nil
}

func rewriteUpgradeImageReferences(value any, original, pinned string) {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "containers" || key == "initContainers" {
				containers, _ := child.([]any)
				for _, raw := range containers {
					container, _ := raw.(map[string]any)
					image, _ := container["image"].(string)
					parsed, err := reference.ParseNormalizedNamed(image)
					if err == nil && reference.TagNameOnly(parsed).String() == original {
						container["image"] = pinned
					}
				}
			}
			if key != "data" && key != "binaryData" && key != "stringData" {
				rewriteUpgradeImageReferences(child, original, pinned)
			}
		}
	case []any:
		for _, child := range value {
			rewriteUpgradeImageReferences(child, original, pinned)
		}
	}
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
