package docker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/porto-containers/porto/internal/runtimes"
)

// openInputStartScript starts a container through nerdctl's attached start in its own session,
// which keeps a non-terminal container's input FIFO open after this script returns. nerdctl's
// detached start gives such containers no input, so 'docker run -d -i ... bash' exited at once.
// It returns when the container runs, or with nerdctl's error when the start fails.
const openInputStartScript = `id="$1"
errors="$(mktemp)"
setsid nerdctl start --attach --interactive "$id" </dev/null >/dev/null 2>"$errors" &
starter=$!
while kill -0 "$starter" 2>/dev/null; do
  if [ "$(nerdctl inspect --format '{{.State.Status}}' "$id" 2>/dev/null)" = running ]; then
    rm -f "$errors"
    exit 0
  fi
  sleep 0.1
done
wait "$starter"
result=$?
cat "$errors" >&2
rm -f "$errors"
exit "$result"`

const openInputStartTimeout = 2 * time.Minute

// openInputCandidate reads the live inventory's labels so ordinary starts never pay for an extra
// inspect. Without an inventory, as during engine recovery, starts keep the regular path.
func (m *Manager) openInputCandidate(id string) bool {
	inventory := m.activeContainerInventory()
	if inventory == nil {
		return false
	}
	container, err := findSnapshotContainer(inventory.snapshotValue(), id)
	if err != nil {
		return false
	}
	return container.Labels[portoOpenStdinLabel] == "true" && container.Labels[portoTTYLabel] != "true"
}

// startWithOpenInput handles start and restart of stopped non-terminal compatibility containers
// created with -i. It reports false when the regular start path applies.
func (m *Manager) startWithOpenInput(ctx context.Context, id, action string, timeout int) (bool, error) {
	if m.IsNativeWindows() || !m.openInputCandidate(id) {
		return false, nil
	}
	details, err := m.containerAttachDetails(ctx, id)
	if err != nil {
		return false, nil
	}
	if details.Managed || details.TTY || !details.OpenStdin || (action == "start" && details.State.active()) {
		return false, nil
	}
	backend, err := m.backendForCommand(ctx)
	if err != nil {
		return true, err
	}
	var command runtimes.Command
	switch {
	case backend.limaInstance != "":
		command = runtimes.Command{Name: "limactl", Args: []string{"shell", "--workdir=/", backend.limaInstance, "--", "sh", "-c", openInputStartScript, "porto-open-input", details.ID}}
	case backend.name == "nerdctl" && len(backend.prefix) == 0:
		command = runtimes.Command{Name: "sh", Args: []string{"-c", openInputStartScript, "porto-open-input", details.ID}}
	default:
		return false, nil
	}
	if action == "restart" && details.State.active() {
		args := []string{"stop"}
		if timeout > 0 {
			args = append(args, "--time", strconv.Itoa(timeout))
		}
		if _, err := m.runWithTimeout(ctx, time.Minute, "stop Porto container for restart", nil, append(args, details.ID)...); err != nil {
			return true, err
		}
	}
	if _, err := m.runBufferedCommand(ctx, openInputStartTimeout, "start Porto container with open input", command); err != nil {
		return true, fmt.Errorf("start Porto container with open input: %w", err)
	}
	m.invalidateContainerInventory()
	return true, nil
}
