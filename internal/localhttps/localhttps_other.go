//go:build !darwin

package localhttps

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("trusted portless HTTPS setup is supported only on macOS")

type Status struct {
	Installed bool `json:"installed"`
	Listening bool `json:"listening"`
	Trusted   bool `json:"trusted"`
}

func Install(_, _ string) error { return errUnsupported }
func Uninstall() error          { return errUnsupported }
func Snapshot(string) Status    { return Status{} }
func Trust(string) error        { return errUnsupported }
func Untrust(string) error      { return errUnsupported }
func Trusted(string) bool       { return false }
func RootInstall(string) error  { return errUnsupported }
func RootUninstall() error      { return errUnsupported }
func Run(context.Context) error { return errUnsupported }
