package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"gopkg.in/yaml.v3"
)

type EngineLimits struct {
	CPUs      int  `json:"cpus"`
	MemoryMiB int  `json:"memoryMiB"`
	SwapMiB   *int `json:"swapMiB"`
	DiskGiB   int  `json:"diskGiB"`
}

type EngineResourceStatus struct {
	Limits          EngineLimits         `json:"limits"`
	Current         *EngineConfiguration `json:"current,omitempty"`
	Supported       bool                 `json:"supported"`
	RestartRequired bool                 `json:"restartRequired"`
	Message         string               `json:"message,omitempty"`
}

func (m *Manager) EngineResourceStatus(ctx context.Context) (EngineResourceStatus, error) {
	limits, err := m.EngineLimits()
	status := EngineResourceStatus{Limits: limits}
	if err != nil {
		return status, err
	}
	state, err := m.readEngineState()
	if errors.Is(err, os.ErrNotExist) {
		_, providerErr := m.lookPath("limactl")
		status.Supported = providerErr == nil
		status.Message = "Budgets apply when the owned Lima engine is created"
		return status, nil
	}
	if err != nil {
		return status, err
	}
	if state.Mode != "lima" {
		status.Message = "External/native containerd is administrator-owned; global VM budgets are unavailable"
		return status, nil
	}
	current, err := m.engineConfiguration(ctx, state.Instance)
	if err != nil {
		return status, err
	}
	status.Current, status.Supported = &current, current.VMType == "qemu" || current.VMType == "vz"
	status.RestartRequired = limits.CPUs > 0 && limits.CPUs != current.CPUs ||
		limits.MemoryMiB > 0 && int64(limits.MemoryMiB)*(1<<20) != current.Memory ||
		limits.DiskGiB > 0 && int64(limits.DiskGiB)*(1<<30) != current.Disk
	if limits.SwapMiB != nil {
		data, err := os.ReadFile(filepath.Join(m.stateDir, "resources-applied.json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return status, err
		}
		var applied EngineLimits
		if len(data) > 0 {
			if err := json.Unmarshal(data, &applied); err != nil {
				return status, fmt.Errorf("decode applied engine budgets: %w", err)
			}
		}
		status.RestartRequired = status.RestartRequired || !reflect.DeepEqual(limits.SwapMiB, applied.SwapMiB)
	}
	return status, nil
}

func (limits EngineLimits) Validate() error {
	if limits.CPUs < 0 || limits.CPUs > 128 ||
		limits.MemoryMiB < 0 || limits.MemoryMiB != 0 && limits.MemoryMiB < 512 || limits.MemoryMiB > 1048576 ||
		limits.DiskGiB < 0 || limits.DiskGiB > 10240 ||
		limits.SwapMiB != nil && (*limits.SwapMiB < 0 || *limits.SwapMiB > 65536) {
		return errors.New("engine budgets require CPUs=0..128, memory=0 or 512..1048576 MiB, disk=0..10240 GiB, and swap=0..65536 MiB; zero CPU/memory/disk preserves provider defaults")
	}
	return nil
}

func (m *Manager) EngineLimits() (EngineLimits, error) {
	var limits EngineLimits
	if m.stateDir == "" {
		return limits, nil
	}
	data, err := os.ReadFile(filepath.Join(m.stateDir, "resources.json"))
	if errors.Is(err, os.ErrNotExist) {
		return limits, nil
	}
	if err != nil {
		return limits, err
	}
	if err := json.Unmarshal(data, &limits); err != nil {
		return limits, fmt.Errorf("decode engine resource budgets: %w", err)
	}
	return limits, limits.Validate()
}

func (m *Manager) SetEngineLimits(ctx context.Context, limits EngineLimits) error {
	if m.stateDir == "" {
		return fmt.Errorf("%w: persistent engine configuration is unavailable", ErrUnavailable)
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	state, err := m.readEngineState()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if state.Mode != "lima" {
			return fmt.Errorf("%w: global VM budgets cannot be imposed on administrator-owned containerd", ErrUnsupported)
		}
		configuration, err := m.engineConfiguration(ctx, state.Instance)
		if err != nil {
			return err
		}
		if _, err := engineLimitsExpression(configuration, nil, limits, state.OwnerID); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(limits, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.stateDir, "resources.json"), append(data, '\n'), 0o600)
}

func (m *Manager) resourceCreateArguments(owner string) ([]string, error) {
	limits, err := m.EngineLimits()
	if err != nil {
		return nil, err
	}
	var arguments []string
	if limits.CPUs > 0 {
		arguments = append(arguments, "--cpus="+strconv.Itoa(limits.CPUs))
	}
	if limits.MemoryMiB > 0 {
		arguments = append(arguments, "--memory="+strconv.FormatFloat(float64(limits.MemoryMiB)/1024, 'f', -1, 64))
	}
	if limits.DiskGiB > 0 {
		arguments = append(arguments, "--disk="+strconv.Itoa(limits.DiskGiB))
	}
	if limits.SwapMiB != nil {
		expression, err := engineLimitsExpression(EngineConfiguration{VMType: "qemu"}, nil, EngineLimits{SwapMiB: limits.SwapMiB}, owner)
		if err != nil {
			return nil, err
		}
		arguments = append(arguments, "--set="+expression)
	}
	return arguments, nil
}

func engineLimitsExpression(current EngineConfiguration, original []byte, limits EngineLimits, owner string) (string, error) {
	if err := limits.Validate(); err != nil {
		return "", err
	}
	if current.VMType != "qemu" && current.VMType != "vz" {
		return "", fmt.Errorf("%w: resource budgets are unavailable for VM driver %q", ErrUnsupported, current.VMType)
	}
	if limits.DiskGiB > 0 && int64(limits.DiskGiB)*(1<<30) < current.Disk {
		return "", errors.New("engine disks cannot be shrunk; existing disks and data were not modified")
	}
	expressions := []string{"."}
	if limits.CPUs > 0 {
		expressions = append(expressions, ".cpus = "+strconv.Itoa(limits.CPUs))
	}
	if limits.MemoryMiB > 0 {
		expressions = append(expressions, fmt.Sprintf(`.memory = "%dMiB"`, limits.MemoryMiB))
	}
	if limits.DiskGiB > 0 {
		expressions = append(expressions, fmt.Sprintf(`.disk = "%dGiB"`, limits.DiskGiB))
	}
	if limits.SwapMiB != nil {
		var configuration struct {
			Provision []map[string]any `yaml:"provision"`
		}
		if err := yaml.Unmarshal(original, &configuration); err != nil {
			return "", fmt.Errorf("read existing guest provisions: %w", err)
		}
		provisions := make([]map[string]any, 0, len(configuration.Provision)+1)
		for _, provision := range configuration.Provision {
			script, _ := provision["script"].(string)
			if !strings.HasPrefix(script, "# porto-engine-swap\n") {
				provisions = append(provisions, provision)
			}
		}
		script := "# porto-engine-swap\nset -eu\nsize=" + strconv.Itoa(*limits.SwapMiB) +
			"\nowner=" + shellJoin([]string{owner}) + "\n" + engineSwapScript
		provisions = append(provisions, map[string]any{"mode": "system", "script": script})
		data, err := json.Marshal(provisions)
		if err != nil {
			return "", err
		}
		literal := `("` + base64.StdEncoding.EncodeToString(data) + `" | @base64d | from_json)`
		if len(original) == 0 {
			expressions = append(expressions, `.provision = (.provision // []) + `+literal)
		} else {
			expressions = append(expressions, `.provision = `+literal)
		}
	}
	return strings.Join(expressions, " | "), nil
}

const engineSwapScript = `root=/var/lib/porto
marker="$root/engine-swap.owner"
mkdir -p "$root"
if [ -e "$marker" ]; then
  [ ! -L "$marker" ] && [ "$(cat "$marker")" = "$owner" ] || {
    echo "Porto swap ownership changed; no swap configuration was replaced" >&2
    exit 1
  }
else
  (umask 077; printf '%s\n' "$owner" > "$marker")
fi
for active in $(swapon --show=NAME --noheadings --raw); do
  case "$active" in "$root"/engine-swap.*MiB) ;; *)
    echo "Unmanaged guest swap prevents enforcing a Porto swap budget: $active" >&2
    exit 1 ;;
  esac
done
if [ "$size" -gt 0 ]; then
  target="$root/engine-swap.${size}MiB"
  [ ! -L "$target" ] || { echo "Owned swap target is a symlink" >&2; exit 1; }
  if [ ! -e "$target" ]; then
    temporary="$(mktemp "$root/.engine-swap.XXXXXX")"
    trap 'rm -f "$temporary"' EXIT
    chmod 0600 "$temporary"
    fallocate -l "${size}MiB" "$temporary"
    mkswap "$temporary"
    mv -n "$temporary" "$target"
  fi
  [ -f "$target" ] && [ "$(stat -c %s "$target")" -eq "$((size*1024*1024))" ] || {
    echo "Owned swap size is inconsistent; no existing swap file was overwritten" >&2
    exit 1
  }
  if ! swapon --show=NAME --noheadings --raw | grep -Fx "$target" >/dev/null; then swapon "$target"; fi
fi
for active in $(swapon --show=NAME --noheadings --raw); do
  if [ "$size" = 0 ] || [ "$active" != "$target" ]; then
    swapoff "$active"
    rm -f -- "$active"
  fi
done
`

const engineSwapPreflight = `set -eu
for command in swapon swapoff mkswap fallocate stat; do
  command -v "$command" >/dev/null || { echo "Guest swap budget requires $command" >&2; exit 1; }
done
case "$(stat -f -c %T /var/lib)" in ext2/ext3|xfs) ;; *)
  echo "Guest filesystem does not support Porto's verified swap-file mechanism" >&2
  exit 1 ;;
esac
for active in $(swapon --show=NAME --noheadings --raw); do
  case "$active" in /var/lib/porto/engine-swap.*MiB) ;; *)
    echo "Unmanaged guest swap prevents changing the budget: $active" >&2
    exit 1 ;;
  esac
done
`

func (m *Manager) ApplyEngineLimits(ctx context.Context, restartConfirmed bool) error {
	limits, err := m.EngineLimits()
	if err != nil {
		return err
	}
	state, err := m.readEngineState()
	if err != nil {
		return err
	}
	return m.editEngineConfiguration(ctx, restartConfirmed, func(current EngineConfiguration, original []byte) (string, error) {
		return engineLimitsExpression(current, original, limits, state.OwnerID)
	}, func(ctx context.Context) error {
		if limits.SwapMiB == nil {
			return nil
		}
		_, err := m.runCommand(ctx, 30*time.Second, "preflight guest swap budget", nil,
			"limactl", "shell", "--workdir=/", state.Instance, "--", "sudo", "-n", "sh", "-c", engineSwapPreflight)
		return err
	}, func(ctx context.Context) error {
		data, err := json.Marshal(limits)
		if err != nil {
			return err
		}
		return config.WriteFileAtomic(filepath.Join(m.stateDir, "resources-applied.json"), data, 0o600)
	})
}
