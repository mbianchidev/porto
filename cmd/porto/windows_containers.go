package main

import (
	"context"
	"errors"
	"flag"

	"github.com/mbianchidev/porto/internal/windowscontainers"
)

func windowsContainersCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	action := args[0]
	switch action {
	case "status":
		if len(args) != 1 {
			return errors.New("usage: porto docker windows status")
		}
		return writeOutput(windowscontainers.Snapshot(context.Background()))
	case "install", "start", "stop", "remove", "context":
		flags := flag.NewFlagSet("docker windows "+action, flag.ContinueOnError)
		confirm := flags.Bool("confirm", false, "explicitly change native service/context state without deleting either backend's stores")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || ((action == "stop" || action == "remove" || action == "context") && !*confirm) {
			return errors.New("usage: porto docker windows install|start|stop|remove|context [--confirm]; stop, remove and context require --confirm")
		}
		if err := windowscontainers.Action(context.Background(), action); err != nil {
			return err
		}
		return writeOutput(windowscontainers.Snapshot(context.Background()))
	default:
		return errors.New("usage: porto docker windows status|install|start|stop|remove|context")
	}
}
