package main

import (
	"errors"
	"flag"
	"net/url"
	"os"
	"strconv"
)

func dockerFilesCmd(args []string) error {
	if len(args) == 0 {
		return runtimeGET("/api/files/attachments")
	}
	if len(args) == 2 && args[0] == "detach" {
		return api("DELETE", "/api/files/attachments/"+url.PathEscape(args[1]), nil, os.Stdout)
	}
	flags := flag.NewFlagSet("files", flag.ContinueOnError)
	writable := flags.Bool("writable", false, "explicit direct native writes; never images")
	confirm := flags.Bool("confirm", false, "opt in to the native host filesystem bridge")
	if err := parseInterspersed(flags, args, map[string]bool{"writable": true, "confirm": true}); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: porto docker files <container|image|volume|vm> <name> [--writable] --confirm; files detach <attachment-id>")
	}
	kind, name := flags.Arg(0), flags.Arg(1)
	if !*confirm {
		query := url.Values{"kind": {kind}, "name": {name}, "writable": {strconv.FormatBool(*writable)}}
		return runtimeGET("/api/files/capabilities?" + query.Encode())
	}
	return runtimePOST("/api/files/attachments", map[string]any{"kind": kind, "name": name, "writable": *writable, "confirm": true})
}
