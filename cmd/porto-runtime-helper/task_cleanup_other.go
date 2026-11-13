//go:build !linux

package main

import (
	"context"
	"errors"
)

func stopLegacyHealthUnits(context.Context, string) error {
	return errors.New("stopped-task health cleanup requires the Linux backend")
}
