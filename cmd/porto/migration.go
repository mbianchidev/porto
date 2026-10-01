package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
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
	confirm := flags.Bool("confirm", false, "execute a conflict-free dry run")
	sensitive := flags.Bool("include-sensitive-environment", false, "explicitly select local sensitive environment transfer")
	helpers := flags.Bool("allow-source-helper", false, "explicitly allow empty-image/stopped-container readonly access for unattached volumes; helpers are never started")
	if err := parseInterspersed(flags, args, map[string]bool{"confirm": true, "include-sensitive-environment": true, "allow-source-helper": true}); err != nil {
		return err
	}
	if *context == "" || flags.NArg() != 0 {
		return errors.New("usage: porto docker migrate --context <name> [--objects image:ref,volume:name,network:name,container:name] [--allow-source-helper] [--confirm]")
	}
	var output bytes.Buffer
	if err := api("GET", "/api/docker/migration/inventory?context="+url.QueryEscape(*context), nil, &output); err != nil {
		return err
	}
	var inventory portodocker.MigrationInventory
	if err := json.Unmarshal(output.Bytes(), &inventory); err != nil {
		return err
	}
	if *objects == "" {
		return writeOutput(inventory)
	}
	request := dataops.Request{Action: "migration", Context: *context, IncludeSensitive: *sensitive, AllowSourceHelper: *helpers}
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
	return runtimePOST("/api/data/operations", preview.Request)
}
