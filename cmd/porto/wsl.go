package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/wsl"
)

type repeatedFlag []string

func (values *repeatedFlag) String() string         { return "" }
func (values *repeatedFlag) Set(value string) error { *values = append(*values, value); return nil }

func wslCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	if args[0] == "dial-stdio" || args[0] == "credential" {
		options := flag.NewFlagSet("wsl transport", flag.ContinueOnError)
		distribution := options.String("distribution", "", "selected distribution")
		owner := options.String("owner", "", "integration ownership")
		home := options.String("home", "", "Windows Porto configuration directory")
		action := options.String("action", "", "credential action")
		if err := options.Parse(args[1:]); err != nil {
			return err
		}
		if *home == "" || !filepath.IsAbs(*home) || len(options.Args()) != 0 {
			return errors.New("WSL transport requires its absolute Windows configuration directory")
		}
		manager := wsl.New(*home, nil)
		if _, err := manager.Authorize(context.Background(), *distribution, *owner); err != nil {
			return err
		}
		if args[0] == "credential" {
			return wsl.NewCredentialVault(*home).Handle(*action, os.Stdin, os.Stdout)
		}
		endpoint, err := config.DockerSocketPath()
		if err != nil {
			return err
		}
		return wsl.PipeStdio(context.Background(), endpoint, os.Stdin, os.Stdout)
	}
	root, err := config.Dir()
	if err != nil {
		return err
	}
	manager := wsl.New(root, nil)
	switch args[0] {
	case "status", "list":
		if len(args) != 1 {
			return errors.New("usage: porto wsl status|list")
		}
		status, err := manager.Snapshot(context.Background())
		if err != nil {
			return err
		}
		return writeOutput(status)
	case "enable":
		options := flag.NewFlagSet("wsl enable", flag.ContinueOnError)
		var roots repeatedFlag
		options.Var(&roots, "mount-root", "additional absolute Linux directory to share with the owned engine (repeatable)")
		if err := options.Parse(args[1:]); err != nil {
			return err
		}
		if len(options.Args()) != 1 {
			return errors.New("usage: porto wsl enable [--mount-root /linux/path] <distribution>")
		}
		integration, err := manager.Enable(context.Background(), options.Args()[0], roots)
		if err != nil {
			return err
		}
		return writeOutput(map[string]any{
			"integration": integration,
			"message":     "Open a new distribution shell. Existing engine mounts require 'porto wsl apply-mounts --restart-engine'; no engine or other distribution was replaced.",
		})
	case "disable":
		if len(args) != 2 {
			return errors.New("usage: porto wsl disable <distribution>")
		}
		return manager.Disable(context.Background(), args[1])
	case "apply-mounts":
		if len(args) != 2 || args[1] != "--restart-engine" {
			return errors.New("usage: porto wsl apply-mounts --restart-engine (interrupts only Porto's owned Linux engine; save running work first)")
		}
		return runtimePOST("/api/wsl/mounts", map[string]bool{"restartEngine": true})
	default:
		return errors.New("usage: porto wsl status|enable|disable|apply-mounts")
	}
}
