package docker

import (
	"context"
	"fmt"
	"strconv"
)

func (m *Manager) UpdateContainer(ctx context.Context, id string, update ContainerUpdate) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	args := []string{"update"}
	if update.NanoCPUs > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(update.NanoCPUs)/1_000_000_000, 'f', -1, 64))
	}
	if update.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(update.Memory, 10))
	}
	if update.MemorySwap > 0 {
		args = append(args, "--memory-swap", strconv.FormatInt(update.MemorySwap, 10))
	}
	if len(args) == 1 {
		return fmt.Errorf("container update requires at least one supported resource limit")
	}
	args = append(args, id)
	_, err := m.run(ctx, "update Porto container", args...)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}
