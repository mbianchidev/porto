package wslbridge

import (
	"context"
	"errors"
	"net"
	"os"

	"github.com/mbianchidev/porto/internal/process"
	"github.com/mbianchidev/porto/internal/runtimes"
)

type stdioAddress string

func (a stdioAddress) Network() string { return "stdio" }
func (a stdioAddress) String() string  { return string(a) }

func DialWindows(ctx context.Context, configuration Config) (net.Conn, error) {
	command := process.NewCommand(context.WithoutCancel(ctx), "", configuration.WindowsExecutable, configuration.WindowsArguments("dial-stdio")...)
	return runtimes.DialCommand(ctx, "selected WSL Docker pipe", command, stdioAddress(configuration.Distribution), stdioAddress("porto_docker_engine"))
}

func Credential(ctx context.Context, configuration Config, action string) error {
	switch action {
	case "get", "store", "erase", "list":
	default:
		return errors.New("credential action must be get, store, erase, or list")
	}
	args := append(configuration.WindowsArguments("credential"), "--action", action)
	command := process.NewCommand(ctx, "", configuration.WindowsExecutable, args...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}
