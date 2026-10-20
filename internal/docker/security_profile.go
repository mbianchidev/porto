package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (m *Manager) createContainerCommand(ctx context.Context, args []string, optionEnd int) (output []byte, err error) {
	profileIndex := -1
	var profile []byte
	for index := 0; index+1 < optionEnd; index++ {
		if args[index] != "--security-opt" {
			continue
		}
		value := args[index+1]
		if !strings.HasPrefix(value, "seccomp=") || value == "seccomp=unconfined" {
			continue
		}
		if profileIndex >= 0 {
			return nil, errors.New("invalid multiple seccomp profiles")
		}
		profile = []byte(strings.TrimPrefix(value, "seccomp="))
		var document struct {
			DefaultAction string `json:"defaultAction"`
		}
		if err := json.Unmarshal(profile, &document); err != nil || document.DefaultAction == "" {
			return nil, errors.New("invalid seccomp profile: pass profile JSON, not a daemon-host filename")
		}
		profileIndex = index + 1
	}
	if profileIndex < 0 {
		return m.runWithTimeout(ctx, 10*time.Minute, "create Porto container", nil, args...)
	}
	backend, err := m.backend(ctx)
	if err != nil {
		return nil, err
	}
	args = append([]string(nil), args...)
	if backend.limaInstance != "" {
		const script = `set -eu
umask 077
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT
trap 'exit 1' HUP INT TERM
cat > "$profile"
index="$1"; shift
position=0
for argument do
  if [ "$position" = "$index" ]; then
    set -- "$@" "seccomp=$profile"
  else
    set -- "$@" "$argument"
  fi
  shift
  position=$((position + 1))
done
nerdctl "$@"
`
		commandArgs := []string{"shell", "--workdir=/", backend.limaInstance, "--",
			"sh", "-c", script, "porto-seccomp-profile", strconv.Itoa(profileIndex)}
		commandArgs = append(commandArgs, args...)
		return m.runCommand(ctx, 10*time.Minute, "create Porto container with seccomp profile", profile, backend.name, commandArgs...)
	}
	directory, err := os.MkdirTemp("", "porto-seccomp-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(directory)) }()
	filename := filepath.Join(directory, "profile.json")
	if err := os.WriteFile(filename, profile, 0o600); err != nil {
		return nil, fmt.Errorf("stage seccomp profile: %w", err)
	}
	args[profileIndex] = "seccomp=" + filename
	return m.runBackend(ctx, backend, 10*time.Minute, "create Porto container with seccomp profile", nil, args...)
}
