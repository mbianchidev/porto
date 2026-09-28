package docker

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type dockerStructuredMount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Target      string `json:"Target"`
	ReadOnly    bool   `json:"ReadOnly"`
	Consistency string `json:"Consistency"`
	BindOptions *struct {
		Propagation            string `json:"Propagation"`
		NonRecursive           bool   `json:"NonRecursive"`
		CreateMountpoint       bool   `json:"CreateMountpoint"`
		ReadOnlyNonRecursive   bool   `json:"ReadOnlyNonRecursive"`
		ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive"`
	} `json:"BindOptions"`
	VolumeOptions *struct {
		NoCopy       bool              `json:"NoCopy"`
		Labels       map[string]string `json:"Labels"`
		Subpath      string            `json:"Subpath"`
		DriverConfig *struct {
			Name    string            `json:"Name"`
			Options map[string]string `json:"Options"`
		} `json:"DriverConfig"`
	} `json:"VolumeOptions"`
	TmpfsOptions *struct {
		SizeBytes int64      `json:"SizeBytes"`
		Mode      *uint32    `json:"Mode"`
		Options   [][]string `json:"Options"`
	} `json:"TmpfsOptions"`
}

func structuredMountArguments(
	mounts []dockerStructuredMount,
	existingTmpfs map[string]string,
) ([]string, map[string]string, error) {
	volumes := make([]string, 0, len(mounts))
	tmpfs := make(map[string]string, len(existingTmpfs))
	targets := make(map[string]struct{}, len(mounts)+len(existingTmpfs))
	for target, options := range existingTmpfs {
		if err := validateContainerMountTarget(target); err != nil {
			return nil, nil, err
		}
		targets[target] = struct{}{}
		tmpfs[target] = options
	}
	for _, mount := range mounts {
		mount.Type = strings.ToLower(strings.TrimSpace(mount.Type))
		mount.Target = strings.TrimSpace(mount.Target)
		if err := validateContainerMountTarget(mount.Target); err != nil {
			return nil, nil, err
		}
		if _, exists := targets[mount.Target]; exists {
			return nil, nil, fmt.Errorf("duplicate mount target %q", mount.Target)
		}
		targets[mount.Target] = struct{}{}
		switch strings.ToLower(strings.TrimSpace(mount.Consistency)) {
		case "", "default", "consistent":
		default:
			return nil, nil, fmt.Errorf("%w: mount consistency %q", ErrUnsupported, mount.Consistency)
		}
		switch mount.Type {
		case "bind":
			value, err := structuredBindMount(mount)
			if err != nil {
				return nil, nil, err
			}
			volumes = append(volumes, value)
		case "volume":
			value, err := structuredVolumeMount(mount)
			if err != nil {
				return nil, nil, err
			}
			volumes = append(volumes, value)
		case "tmpfs":
			options, err := structuredTmpfsMount(mount)
			if err != nil {
				return nil, nil, err
			}
			tmpfs[mount.Target] = options
		default:
			return nil, nil, fmt.Errorf("%w: mount type %q", ErrUnsupported, mount.Type)
		}
	}
	return volumes, tmpfs, nil
}

func structuredBindMount(mount dockerStructuredMount) (string, error) {
	source := strings.TrimSpace(mount.Source)
	if source == "" {
		return "", errors.New("bind mount source is required")
	}
	if strings.ContainsAny(source, "\r\n\x00") {
		return "", errors.New("invalid bind mount source")
	}
	if mount.VolumeOptions != nil || mount.TmpfsOptions != nil {
		return "", errors.New("invalid bind mount options")
	}
	options := make([]string, 0, 2)
	if mount.ReadOnly {
		options = append(options, "ro")
	}
	if bind := mount.BindOptions; bind != nil {
		if bind.NonRecursive || bind.CreateMountpoint || bind.ReadOnlyNonRecursive || bind.ReadOnlyForceRecursive {
			return "", fmt.Errorf("%w: recursive bind mount controls", ErrUnsupported)
		}
		propagation := strings.ToLower(strings.TrimSpace(bind.Propagation))
		switch propagation {
		case "", "rprivate", "private", "rshared", "shared", "rslave", "slave":
			if propagation != "" {
				options = append(options, propagation)
			}
		default:
			return "", fmt.Errorf("%w: bind propagation %q", ErrUnsupported, bind.Propagation)
		}
	}
	return mountValue(source, mount.Target, options), nil
}

func structuredVolumeMount(mount dockerStructuredMount) (string, error) {
	if mount.BindOptions != nil || mount.TmpfsOptions != nil {
		return "", errors.New("invalid volume mount options")
	}
	source := strings.TrimSpace(mount.Source)
	if source != "" {
		if err := validateObjectID(source); err != nil {
			return "", fmt.Errorf("volume source: %w", err)
		}
	}
	options := make([]string, 0, 2)
	if mount.ReadOnly {
		options = append(options, "ro")
	}
	if volume := mount.VolumeOptions; volume != nil {
		if len(volume.Labels) > 0 || volume.DriverConfig != nil || strings.TrimSpace(volume.Subpath) != "" {
			return "", fmt.Errorf("%w: volume labels, driver configuration, and subpaths", ErrUnsupported)
		}
		if volume.NoCopy {
			options = append(options, "nocopy")
		}
	}
	return mountValue(source, mount.Target, options), nil
}

func structuredTmpfsMount(mount dockerStructuredMount) (string, error) {
	if strings.TrimSpace(mount.Source) != "" || mount.BindOptions != nil || mount.VolumeOptions != nil {
		return "", errors.New("invalid tmpfs mount options")
	}
	options := make([]string, 0, 4)
	if mount.ReadOnly {
		options = append(options, "ro")
	}
	if tmpfs := mount.TmpfsOptions; tmpfs != nil {
		if tmpfs.SizeBytes < 0 {
			return "", errors.New("tmpfs size cannot be negative")
		}
		if tmpfs.SizeBytes > 0 {
			options = append(options, "size="+strconv.FormatInt(tmpfs.SizeBytes, 10))
		}
		if tmpfs.Mode != nil {
			options = append(options, "mode="+strconv.FormatUint(uint64(*tmpfs.Mode), 8))
		}
		for _, group := range tmpfs.Options {
			for _, option := range group {
				option = strings.ToLower(strings.TrimSpace(option))
				switch option {
				case "exec", "noexec", "suid", "nosuid", "dev", "nodev":
					options = append(options, option)
				case "":
				default:
					return "", fmt.Errorf("%w: tmpfs option %q", ErrUnsupported, option)
				}
			}
		}
	}
	return strings.Join(options, ","), nil
}

func mountValue(source, target string, options []string) string {
	value := target
	if source != "" {
		value = source + ":" + target
	}
	if len(options) > 0 {
		value += ":" + strings.Join(options, ",")
	}
	return value
}

func validateContainerMountTarget(target string) error {
	if target == "" {
		return errors.New("mount target is required")
	}
	if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, "\r\n\x00:") {
		return fmt.Errorf("invalid mount target %q", target)
	}
	return nil
}
