package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/coreos/go-systemd/v22/dbus"
	godbus "github.com/godbus/dbus/v5"
)

type stoppedHealthUnitManager interface {
	ListUnitsByNamesContext(context.Context, []string) ([]dbus.UnitStatus, error)
	StopUnitContext(context.Context, string, string, chan<- string) (int, error)
}

func stopExistingHealthUnits(ctx context.Context, manager stoppedHealthUnitManager, id string) error {
	names := []string{id + ".timer", id + ".service"}
	units, err := manager.ListUnitsByNamesContext(ctx, names)
	if err != nil {
		return fmt.Errorf("inspect stopped-task health units: %w", err)
	}
	states := make(map[string]dbus.UnitStatus, len(units))
	for _, unit := range units {
		states[unit.Name] = unit
	}
	var result error
	for _, name := range names {
		if ctx.Err() != nil {
			return errors.Join(result, context.Cause(ctx))
		}
		unit, exists := states[name]
		if !exists || unit.LoadState == "not-found" ||
			unit.ActiveState == "inactive" || unit.ActiveState == "failed" {
			continue
		}
		completed := make(chan string, 1)
		if _, err := manager.StopUnitContext(ctx, name, "ignore-dependencies", completed); err != nil {
			if missingHealthUnit(err) {
				continue
			}
			result = errors.Join(result, fmt.Errorf("stop health unit %s: %w", name, err))
			continue
		}
		select {
		case state := <-completed:
			if state != "done" {
				result = errors.Join(result, fmt.Errorf("health unit %s stop returned %q", name, state))
			}
		case <-ctx.Done():
			return errors.Join(result, context.Cause(ctx))
		}
	}
	return result
}

func missingHealthUnit(err error) bool {
	const missing = "org.freedesktop.systemd1.NoSuchUnit"
	var value godbus.Error
	if errors.As(err, &value) {
		return value.Name == missing
	}
	var pointer *godbus.Error
	return errors.As(err, &pointer) && pointer.Name == missing
}
