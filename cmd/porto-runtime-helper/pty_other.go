//go:build !linux

package main

import (
	"context"
	"errors"
	"io"
)

func runPTYCommand(context.Context, []string, io.Reader, io.Writer) error {
	return errors.New("terminal command requires the Linux runtime helper")
}
