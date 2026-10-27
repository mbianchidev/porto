package main

import (
	"errors"
	"flag"
	"os"

	"github.com/mbianchidev/porto/internal/acceleration"
)

func accelerationCmd(args []string) error {
	if len(args) == 0 || (len(args) == 1 && args[0] == "status") {
		return runtimeGET("/api/runtime/acceleration")
	}
	switch args[0] {
	case "configure":
		flags := flag.NewFlagSet("runtime acceleration configure", flag.ContinueOnError)
		policy := flags.String("policy", acceleration.Automatic, "automatic, native-only, rosetta-preferred, or qemu-compatible")
		rosetta := flags.Bool("rosetta", false, "opt in to already-installed Rosetta inside the owned VZ engine")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		settings := acceleration.Settings{Policy: *policy, RosettaEnabled: *rosetta}
		if flags.NArg() != 0 {
			return errors.New("usage: porto runtime acceleration configure [--policy automatic] [--rosetta]")
		}
		if err := acceleration.Validate(settings); err != nil {
			return err
		}
		if !daemonUp() {
			return errors.New("start the Porto daemon before configuring runtime acceleration")
		}
		return api("PUT", "/api/runtime/acceleration", settings, os.Stdout)
	case "apply":
		flags := flag.NewFlagSet("runtime acceleration apply", flag.ContinueOnError)
		restart := flags.Bool("restart-engine", false, "restart the owned idle engine without replacing its disk")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || !*restart {
			return errors.New("usage: porto runtime acceleration apply --restart-engine")
		}
		return runtimePOST("/api/runtime/acceleration/apply", map[string]bool{"restartEngine": true})
	default:
		return errors.New("usage: porto runtime acceleration status|configure|apply")
	}
}
