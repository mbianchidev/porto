package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func dockerDataCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto docker storage|volume|backups|operations <args>")
	}
	switch args[0] {
	case "storage":
		if len(args) == 1 || len(args) == 2 && args[1] == "usage" {
			return runtimeGET("/api/docker/storage")
		}
		if args[1] != "prune" {
			return errors.New("usage: porto docker storage usage|prune --category image|container|volume|network|cache [--confirm]")
		}
		flags := flag.NewFlagSet("storage prune", flag.ContinueOnError)
		category := flags.String("category", "", "one explicit resource category")
		confirm := flags.Bool("confirm", false, "execute the exact preview")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *category == "" {
			return errors.New("an explicit --category is required; broad cleanup is not the default")
		}
		request := dataops.Request{Action: "prune", Categories: []string{*category}}
		var output bytes.Buffer
		if err := api("POST", "/api/docker/storage/preview", request, &output); err != nil {
			return err
		}
		var preview portodocker.PrunePreview
		if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
			return err
		}
		if err := writeOutput(preview); err != nil {
			return err
		}
		if !*confirm {
			return nil
		}
		request.Preview, request.Confirm = preview.Token, true
		return runtimePOST("/api/data/operations", request)
	case "volume":
		return dockerVolumeDataCmd(args[1:])
	case "operations":
		if len(args) == 1 {
			return runtimeGET("/api/data/operations")
		}
		if len(args) == 3 && args[1] == "cancel" {
			id, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil || id < 1 {
				return errors.New("invalid operation ID")
			}
			return api("DELETE", "/api/data/operations/"+strconv.FormatInt(id, 10), nil, os.Stdout)
		}
		if len(args) == 2 {
			return runtimeGET("/api/data/operations/" + url.PathEscape(args[1]))
		}
		return errors.New("usage: porto docker operations [id]|cancel <id>")
	case "backups":
		if len(args) == 1 {
			return runtimeGET("/api/docker/backups")
		}
		if len(args) == 3 && args[1] == "run" {
			return runtimePOST("/api/docker/backups/"+url.PathEscape(args[2])+"/run", nil)
		}
		if len(args) == 4 && args[1] == "remove" && args[3] == "--confirm" {
			return api("DELETE", "/api/docker/backups/"+url.PathEscape(args[2])+"?confirm=true", nil, os.Stdout)
		}
		if args[1] == "schedule" {
			flags := flag.NewFlagSet("backups schedule", flag.ContinueOnError)
			hours := flags.Int("hours", 24, "local backup interval in hours")
			retention := flags.Int("retain", 7, "verified archives to retain")
			directory := flags.String("directory", "", "absolute local archive directory; default Porto state")
			enabled := flags.Bool("enabled", false, "explicitly enable automatic local backups")
			if err := parseInterspersed(flags, args[2:], map[string]bool{"enabled": true}); err != nil {
				return err
			}
			if flags.NArg() != 1 {
				return errors.New("usage: porto docker backups schedule <volume> [--hours 24] [--retain 7] [--directory path] [--enabled]")
			}
			return runtimePOST("/api/docker/backups", map[string]any{
				"id": 0, "resource": map[string]string{"kind": "volume", "name": flags.Arg(0)},
				"enabled": *enabled, "intervalHours": *hours, "retention": *retention, "directory": *directory, "nextRunAt": "",
			})
		}
		return errors.New("usage: porto docker backups [schedule <volume>|run <id>|remove <id> --confirm]")
	default:
		return errors.New("unknown data-management command")
	}
}

func dockerVolumeDataCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto docker volume export|clone|import|restore|empty <args> [--confirm]")
	}
	flags := flag.NewFlagSet("volume "+args[0], flag.ContinueOnError)
	confirm := flags.Bool("confirm", false, "execute the exact volume action preview")
	if err := parseInterspersed(flags, args[1:], map[string]bool{"confirm": true}); err != nil {
		return err
	}
	request := dataops.Request{Action: "volume-" + args[0]}
	switch args[0] {
	case "export":
		if flags.NArg() != 2 {
			return errors.New("usage: porto docker volume export <volume> <local.tar> [--confirm]")
		}
		request.Resource.Name = flags.Arg(0)
		destination, err := filepath.Abs(flags.Arg(1))
		if err != nil {
			return err
		}
		request.Destination = destination
	case "clone":
		if flags.NArg() != 2 {
			return errors.New("usage: porto docker volume clone <source-volume> <new-volume> [--confirm]")
		}
		request.Resource.Name, request.Destination = flags.Arg(0), flags.Arg(1)
	case "import":
		if flags.NArg() != 2 {
			return errors.New("usage: porto docker volume import <local.tar> <new-volume> [--confirm]")
		}
		archive, err := filepath.Abs(flags.Arg(0))
		if err != nil {
			return err
		}
		request.Archive, request.Destination = archive, flags.Arg(1)
	case "restore":
		if flags.NArg() != 2 {
			return errors.New("usage: porto docker volume restore <volume> <local.tar> [--confirm]")
		}
		archive, err := filepath.Abs(flags.Arg(1))
		if err != nil {
			return err
		}
		request.Resource.Name, request.Archive = flags.Arg(0), archive
	case "empty":
		if flags.NArg() != 1 {
			return errors.New("usage: porto docker volume empty <volume> [--confirm]")
		}
		request.Resource.Name = flags.Arg(0)
	default:
		return fmt.Errorf("unknown volume action %q", args[0])
	}
	var output bytes.Buffer
	if err := api("POST", "/api/docker/storage/preview", request, &output); err != nil {
		return err
	}
	var preview portodocker.VolumePreview
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		return err
	}
	if err := writeOutput(preview); err != nil {
		return err
	}
	if !*confirm {
		return nil
	}
	preview.Request.Confirm, preview.Request.Preview = true, preview.Token
	return runtimePOST("/api/data/operations", preview.Request)
}
