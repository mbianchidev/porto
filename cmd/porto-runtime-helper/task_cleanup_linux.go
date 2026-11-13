//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/coreos/go-systemd/v22/dbus"
)

func stopLegacyHealthUnits(ctx context.Context, id string) error {
	// nerdctl does not create systemd health units in rootless mode.
	if os.Geteuid() != 0 || os.Getenv("ROOTLESSKIT_STATE_DIR") != "" {
		return nil
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect systemd health cleanup support: %w", err)
	}
	connection, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("connect stopped-task health cleanup: %w", err)
	}
	defer connection.Close()
	return stopExistingHealthUnits(ctx, connection, id)
}
