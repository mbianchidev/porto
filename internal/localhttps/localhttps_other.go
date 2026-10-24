//go:build !darwin && !linux && !windows

package localhttps

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("trusted portless HTTPS setup requires macOS, Windows, or systemd Linux")

func Install(_, _ string) error           { return errUnsupported }
func Uninstall() error                    { return errUnsupported }
func Snapshot(string) Status              { return Status{Message: errUnsupported.Error()} }
func Trust(string) error                  { return errUnsupported }
func Untrust(string) error                { return errUnsupported }
func Trusted(string) bool                 { return false }
func RootInstall(string, ...string) error { return errUnsupported }
func RootUninstall() error                { return errUnsupported }
func Run(context.Context) error           { return errUnsupported }
