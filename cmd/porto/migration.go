package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
	"strconv"
	"strings"

	"github.com/mbianchidev/porto/internal/dataops"
	portodocker "github.com/mbianchidev/porto/internal/docker"
)

func dockerMigrationCmd(args []string) error {
	if len(args) == 0 || len(args) == 1 && args[0] == "contexts" {
		return runtimeGET("/api/docker/migration/contexts")
	}
	flags := flag.NewFlagSet("docker migrate", flag.ContinueOnError)
	context := flags.String("context", "", "explicit local source Docker context")
	objects := flags.String("objects", "", "comma-separated kind:name selectors")
	destinations := flags.String("destinations", "", "comma-separated kind:source=destination overrides for selected objects")
	resume := flags.Int64("resume", 0, "review saved migration identities and destinations using a fresh dry run")
	confirm := flags.Bool("confirm", false, "execute a conflict-free dry run")
	sensitive := flags.Bool("include-sensitive-environment", false, "explicitly select local sensitive environment transfer")
	helpers := flags.Bool("allow-source-helper", false, "explicitly allow empty-image/stopped-container readonly access for unattached volumes; helpers are never started")
	if err := parseInterspersed(flags, args, map[string]bool{"confirm": true, "include-sensitive-environment": true, "allow-source-helper": true}); err != nil {
		return err
	}
	resumeSelected := false
	flags.Visit(func(value *flag.Flag) {
		if value.Name == "resume" {
			resumeSelected = true
		}
	})
	if resumeSelected && *resume < 1 {
		return errors.New("--resume requires a positive saved migration operation ID")
	}
	if *resume < 0 || flags.NArg() != 0 || *resume == 0 && *context == "" {
		return errors.New("usage: porto docker migrate --context <name> [--objects kind:name,...] [--destinations kind:source=destination,...] [--confirm]; migrate --resume <operation-id> [--confirm]")
	}
	var output bytes.Buffer
	request := dataops.Request{Action: "migration", Context: *context, IncludeSensitive: *sensitive, AllowSourceHelper: *helpers}
	if *resume != 0 {
		if *context != "" || *objects != "" || *destinations != "" {
			return errors.New("--resume cannot be combined with --context, --objects or --destinations; saved identities are not retargeted")
		}
		if err := api("GET", "/api/data/operations/"+strconv.FormatInt(*resume, 10), nil, &output); err != nil {
			return err
		}
		var previous dataops.Operation
		if err := json.Unmarshal(output.Bytes(), &previous); err != nil {
			return err
		}
		if previous.Request.Action != "migration" || previous.Request.Context == "" || len(previous.Request.Selections) == 0 ||
			previous.Status != "failed" && previous.Status != "cancelled" && previous.Status != "interrupted" {
			return errors.New("only a failed, cancelled or interrupted migration with saved selections can be resumed")
		}
		request.Context, request.Selections = previous.Request.Context, previous.Request.Selections
	} else {
		if err := api("GET", "/api/docker/migration/inventory?context="+url.QueryEscape(*context), nil, &output); err != nil {
			return err
		}
		var inventory portodocker.MigrationInventory
		if err := json.Unmarshal(output.Bytes(), &inventory); err != nil {
			return err
		}
		if *objects == "" {
			if *destinations != "" {
				return errors.New("--destinations requires selected --objects")
			}
			return writeOutput(inventory)
		}
		for _, selector := range strings.Split(*objects, ",") {
			kind, name, valid := strings.Cut(selector, ":")
			if !valid {
				return errors.New("migration selectors must use kind:name")
			}
			found := false
			for _, object := range inventory.Objects {
				if object.Kind == kind && object.Name == name {
					request.Selections = append(request.Selections, dataops.Selection{Kind: kind, Name: name, ID: object.ID})
					found = true
					break
				}
			}
			if !found {
				return errors.New("migration selector did not match an exact source inventory object")
			}
		}
	}
	if *destinations != "" {
		seen := make(map[string]bool)
		for _, override := range strings.Split(*destinations, ",") {
			selector, destination, valid := strings.Cut(override, "=")
			kind, name, selectorValid := strings.Cut(selector, ":")
			if !valid || !selectorValid || destination == "" || seen[selector] {
				return errors.New("destination overrides must use unique kind:source=destination selectors")
			}
			seen[selector] = true
			found := false
			for index := range request.Selections {
				if request.Selections[index].Kind == kind && request.Selections[index].Name == name {
					request.Selections[index].Destination = destination
					found = true
				}
			}
			if !found {
				return errors.New("destination override does not match a selected source object")
			}
		}
	}
	output.Reset()
	if err := api("POST", "/api/docker/storage/preview", request, &output); err != nil {
		return err
	}
	var preview portodocker.MigrationPreview
	if err := json.Unmarshal(output.Bytes(), &preview); err != nil {
		return err
	}
	if err := writeOutput(preview); err != nil {
		return err
	}
	if !*confirm {
		return nil
	}
	if len(preview.Conflicts) > 0 {
		return errors.New("migration dry run has conflicts; nothing was created")
	}
	preview.Request.Confirm = true
	preview.Request.Preview = preview.Token
	return runtimePOST("/api/data/operations", preview.Request)
}
