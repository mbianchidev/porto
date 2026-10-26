package main

import (
	"errors"
	"flag"
	"net/url"
	"os"
	"strings"
)

func sharesCmd(args []string) error {
	if len(args) == 0 || len(args) == 1 && args[0] == "status" {
		return runtimeGET("/api/runtime/shares")
	}
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("shares create", flag.ContinueOnError)
		ignores := flags.String("ignore", "node_modules", "comma-separated maintained-tool ignore patterns; VCS directories are always ignored")
		if err := parseInterspersed(flags, args[1:], nil); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: porto shares create <registered-project> [--ignore node_modules]")
		}
		patterns := []string{}
		for _, pattern := range strings.Split(*ignores, ",") {
			if value := strings.TrimSpace(pattern); value != "" {
				patterns = append(patterns, value)
			}
		}
		return runtimePOST("/api/runtime/shares", map[string]any{"project": flags.Arg(0), "ignore": patterns})
	case "mode":
		if len(args) != 3 || args[2] != "bind" && args[2] != "synchronized" {
			return errors.New("usage: porto shares mode <share-id> bind|synchronized")
		}
		if !daemonUp() {
			return errors.New("start the Porto daemon before changing synchronized shares")
		}
		return api("PUT", "/api/runtime/shares/"+url.PathEscape(args[1]), map[string]string{"mode": args[2]}, os.Stdout)
	case "detach":
		if len(args) != 3 || args[2] != "--confirm" {
			return errors.New("usage: porto shares detach <share-id> --confirm; host and guest copies are retained")
		}
		return runtimeDELETE("/api/runtime/shares/" + url.PathEscape(args[1]) + "?confirm=true")
	default:
		return errors.New("usage: porto shares status|create|mode|detach")
	}
}
