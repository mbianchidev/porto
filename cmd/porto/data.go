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
	"slices"
	"strconv"
	"strings"

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
		objects := flags.String("objects", "", "comma-separated exact kind:name selectors")
		confirm := flags.Bool("confirm", false, "execute the exact preview")
		if err := flags.Parse(args[2:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || (*category == "") == (*objects == "") {
			return errors.New("choose exactly one --category or --objects selection; broad cleanup is not the default")
		}
		request := dataops.Request{Action: "prune"}
		var output bytes.Buffer
		if *category != "" {
			if !slices.Contains([]string{"image", "container", "volume", "network", "cache"}, *category) {
				return errors.New("cleanup category must be image, container, volume, network or cache")
			}
			request.Categories = []string{*category}
		} else {
			if err := api("GET", "/api/docker/storage", nil, &output); err != nil {
				return err
			}
			var usage portodocker.StorageUsage
			if err := json.Unmarshal(output.Bytes(), &usage); err != nil {
				return err
			}
			for _, selector := range strings.Split(*objects, ",") {
				kind, name, ok := strings.Cut(selector, ":")
				if !ok || name == "" {
					return errors.New("cleanup selectors must use kind:name")
				}
				found := false
				for _, item := range usage.Resources {
					if item.Resource.Kind == kind && item.Resource.Name == name {
						request.Selections = append(request.Selections, dataops.Selection{Kind: kind, Name: name, ID: item.Resource.ID})
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("cleanup selector %q did not match an exact inventory object", selector)
				}
			}
			output.Reset()
		}
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
		if !*confirm || len(preview.Candidates) == 0 {
			return nil
		}
		preview.Request.Preview, preview.Request.Confirm = preview.Token, true
		return runtimePOST("/api/data/operations", preview.Request)
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
			if len(args) == 3 && args[1] == "watch" {
				id, err := strconv.ParseInt(args[2], 10, 64)
				if err != nil || id < 1 {
					return errors.New("invalid operation ID")
				}
				return watchDataOperation(id)
			}
			return api("DELETE", "/api/data/operations/"+strconv.FormatInt(id, 10), nil, os.Stdout)
		}
		if len(args) == 2 {
			return runtimeGET("/api/data/operations/" + url.PathEscape(args[1]))
		}
		return errors.New("usage: porto docker operations [id]|watch <id>|cancel <id>")
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
		if len(args) == 3 && (args[1] == "pause" || args[1] == "resume") {
			schedule, err := readBackupSchedule(args[2])
			if err != nil {
				return err
			}
			schedule.Enabled = args[1] == "resume"
			return runtimePOST("/api/docker/backups", schedule)
		}
		if args[1] == "update" {
			flags := flag.NewFlagSet("backups update", flag.ContinueOnError)
			hours := flags.Int("hours", 24, "local backup interval in hours")
			retention := flags.Int("retain", 7, "verified archives to retain")
			directory := flags.String("directory", "", "backup folder inside Porto state")
			if err := parseInterspersed(flags, args[2:], nil); err != nil {
				return err
			}
			if flags.NArg() != 1 {
				return errors.New("usage: porto docker backups update <id> [--hours N] [--retain N] [--directory path]")
			}
			schedule, err := readBackupSchedule(flags.Arg(0))
			if err != nil {
				return err
			}
			flags.Visit(func(value *flag.Flag) {
				switch value.Name {
				case "hours":
					schedule.IntervalHours = *hours
				case "retain":
					schedule.Retention = *retention
				case "directory":
					schedule.Directory = *directory
				}
			})
			return runtimePOST("/api/docker/backups", schedule)
		}
		if args[1] == "schedule" {
			flags := flag.NewFlagSet("backups schedule", flag.ContinueOnError)
			hours := flags.Int("hours", 24, "local backup interval in hours")
			retention := flags.Int("retain", 7, "verified archives to retain")
			directory := flags.String("directory", "", "backup folder inside Porto state; default source-specific folder")
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
		return errors.New("usage: porto docker backups [schedule <volume>|update <id>|pause <id>|resume <id>|run <id>|remove <id> --confirm]")
	default:
		return errors.New("unknown data-management command")
	}
}

func dockerVolumeDataCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: porto docker volume export|clone|import|restore|empty|remove <args> [--confirm]")
	}
	flags := flag.NewFlagSet("volume "+args[0], flag.ContinueOnError)
	confirm := flags.Bool("confirm", false, "execute the exact volume action preview")
	if err := parseInterspersed(flags, args[1:], map[string]bool{"confirm": true}); err != nil {
		return err
	}
	request := dataops.Request{Action: "volume-" + args[0]}
	localExport := ""
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
		if _, err := os.Lstat(destination); err == nil {
			return fmt.Errorf("local export destination already exists: %s", destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent, err := os.Stat(filepath.Dir(destination))
		if err != nil {
			return err
		}
		if !parent.IsDir() {
			return errors.New("local export destination parent is not a directory")
		}
		localExport = destination
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
		uploaded, err := uploadLocalVolumeArchive(archive)
		if err != nil {
			return err
		}
		request.Archive, request.Destination = uploaded.Path, flags.Arg(1)
	case "restore":
		if flags.NArg() != 2 {
			return errors.New("usage: porto docker volume restore <volume> <local.tar> [--confirm]")
		}
		archive, err := filepath.Abs(flags.Arg(1))
		if err != nil {
			return err
		}
		uploaded, err := uploadLocalVolumeArchive(archive)
		if err != nil {
			return err
		}
		request.Resource.Name, request.Archive = flags.Arg(0), uploaded.Path
	case "empty", "remove":
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: porto docker volume %s <volume> [--confirm]", args[0])
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
	if localExport != "" {
		fmt.Fprintf(os.Stdout, "Local download destination: %s (created by this CLI, not the daemon)\n", localExport)
	}
	if !*confirm {
		return nil
	}
	preview.Request.Confirm, preview.Request.Preview = true, preview.Token
	if localExport != "" {
		var response bytes.Buffer
		if err := api("POST", "/api/data/operations", preview.Request, &response); err != nil {
			return err
		}
		var operation dataops.Operation
		if err := json.Unmarshal(response.Bytes(), &operation); err != nil {
			return err
		}
		return downloadCompletedVolumeExport(operation.ID, localExport)
	}
	return runtimePOST("/api/data/operations", preview.Request)
}

func readBackupSchedule(value string) (dataops.Schedule, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return dataops.Schedule{}, errors.New("invalid backup schedule ID")
	}
	var output bytes.Buffer
	if err := api("GET", "/api/docker/backups", nil, &output); err != nil {
		return dataops.Schedule{}, err
	}
	var schedules []dataops.Schedule
	if err := json.Unmarshal(output.Bytes(), &schedules); err != nil {
		return dataops.Schedule{}, err
	}
	for _, schedule := range schedules {
		if schedule.ID == id {
			return schedule, nil
		}
	}
	return dataops.Schedule{}, errors.New("backup schedule not found")
}
