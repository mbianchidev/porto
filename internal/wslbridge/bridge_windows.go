//go:build windows

package wslbridge

import (
	"context"
	"errors"
	"net"
)

var errLinuxHelper = errors.New("the WSL socket helper must run inside the selected Linux distribution")

func Serve(context.Context, Config, func(context.Context) (net.Conn, error)) error {
	return errLinuxHelper
}
func Ensure(context.Context, string, Config) error { return errLinuxHelper }
func Stop(context.Context, Config) error           { return errLinuxHelper }
func Endpoint(configuration Config) string         { return "unix://" + configuration.Socket }
