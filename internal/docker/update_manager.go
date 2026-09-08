package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

func (m *Manager) UpdateContainer(ctx context.Context, id string, update ContainerUpdate) error {
	if err := validateObjectID(id); err != nil {
		return err
	}
	if update.Healthcheck != nil {
		if update.Memory != 0 || update.MemorySwap != 0 || update.NanoCPUs != 0 {
			return fmt.Errorf("%w: healthcheck and resource updates cannot be combined", ErrUnsupported)
		}
		if err := validateHealthcheck(update.Healthcheck); err != nil {
			return err
		}
		return m.updateContainerHealth(ctx, id, update.Healthcheck)
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
	if handled, err := m.withContainerOperations(ctx, func(operations containerOperations) error {
		return operations.UpdateResources(ctx, id, update)
	}); handled {
		if err == nil {
			m.invalidateContainerInventory()
		}
		return err
	}
	args = append(args, id)
	_, err := m.run(ctx, "update Porto container", args...)
	if err == nil {
		m.invalidateContainerInventory()
	}
	return err
}

func (m *Manager) updateContainerHealth(
	ctx context.Context,
	id string,
	healthcheck *ContainerHealthcheck,
) error {
	connector := m.operationsConnector
	if connector == nil {
		return fmt.Errorf(
			"%w: direct healthcheck updates require containerd health lifecycle support",
			ErrUnsupported,
		)
	}
	operations, err := connector(ctx)
	if err != nil {
		return err
	}
	updateErr := operations.UpdateHealth(ctx, id, healthcheck)
	closeErr := operations.Close()
	if updateErr == nil {
		m.invalidateContainerInventory()
	}
	return errors.Join(updateErr, closeErr)
}
