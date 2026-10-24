package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/wslbridge"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "porto-wsl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println(config.Version)
		return nil
	}
	if len(args) < 2 {
		return errors.New("usage: porto-wsl ensure|serve|stop|endpoint|credential <configuration> [credential action]")
	}
	configuration, err := wslbridge.Load(args[1])
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch args[0] {
	case "ensure":
		return wslbridge.Ensure(ctx, args[1], configuration)
	case "serve":
		return wslbridge.Serve(ctx, configuration, func(ctx context.Context) (net.Conn, error) { return wslbridge.DialWindows(ctx, configuration) })
	case "stop":
		return wslbridge.Stop(ctx, configuration)
	case "endpoint":
		fmt.Println(wslbridge.Endpoint(configuration))
		return nil
	case "credential":
		if len(args) != 3 {
			return errors.New("credential action is required")
		}
		return wslbridge.Credential(ctx, configuration, args[2])
	default:
		return fmt.Errorf("unknown WSL helper action %q", args[0])
	}
}
