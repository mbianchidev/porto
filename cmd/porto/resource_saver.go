package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net/url"
	"os"
	"strings"

	portodocker "github.com/mbianchidev/porto/internal/docker"
	"github.com/mbianchidev/porto/internal/resourcesaver"
)

func resourceSaverCmd(args []string) error {
	if len(args) == 0 || len(args) == 1 && args[0] == "status" {
		return runtimeGET("/api/runtime/resource-saver")
	}
	if !daemonUp() {
		return errors.New("start the Porto daemon before configuring Resource Saver")
	}
	switch args[0] {
	case "configure":
		var document bytes.Buffer
		if err := api("GET", "/api/runtime/resource-saver", nil, &document); err != nil {
			return err
		}
		var current struct {
			State resourcesaver.State `json:"state"`
		}
		if err := json.Unmarshal(document.Bytes(), &current); err != nil {
			return err
		}
		configuration := current.State.Config
		flags := flag.NewFlagSet("resource-saver configure", flag.ContinueOnError)
		flags.StringVar(&configuration.Mode, "mode", configuration.Mode, "disabled, engine-idle-suspend, container-pause, or container-scale-to-zero")
		flags.IntVar(&configuration.IdleSeconds, "idle", configuration.IdleSeconds, "idle timeout in seconds")
		flags.IntVar(&configuration.WakeSeconds, "wake", configuration.WakeSeconds, "wake/readiness timeout in seconds")
		flags.IntVar(&configuration.MaxPending, "pending", configuration.MaxPending, "maximum retained requests")
		flags.Int64Var(&configuration.MaxBodyBytes, "max-body", configuration.MaxBodyBytes, "maximum queued HTTP body bytes")
		flags.BoolVar(&configuration.SuspendEngine, "suspend-engine", configuration.SuspendEngine, "also suspend the wholly idle owned engine")
		exclusions := flags.String("exclude", strings.Join(configuration.Exclusions, ","), "comma-separated application/container name exclusions")
		overrides := flags.String("overrides", "", "JSON application/container policy overrides")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("usage: porto resource-saver configure [--mode disabled] [--idle 300] [--wake 60] [--suspend-engine]")
		}
		configuration.Exclusions = nil
		for _, exclusion := range strings.Split(*exclusions, ",") {
			if value := strings.TrimSpace(exclusion); value != "" {
				configuration.Exclusions = append(configuration.Exclusions, value)
			}
		}
		if *overrides != "" {
			if err := json.Unmarshal([]byte(*overrides), &configuration.Overrides); err != nil {
				return err
			}
		}
		if err := resourcesaver.Validate(configuration); err != nil {
			return err
		}
		return api("PUT", "/api/runtime/resource-saver", configuration, os.Stdout)
	case "container":
		flags := flag.NewFlagSet("resource-saver container", flag.ContinueOnError)
		mode := flags.String("mode", "exclude", "pause, stop, or exclude")
		stateful := flags.Bool("stateful", false, "explicitly allow suspension of this mounted/stateful workload")
		if err := parseInterspersed(flags, args[1:], map[string]bool{"stateful": true}); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("usage: porto resource-saver container <container> --mode pause|stop|exclude [--stateful]")
		}
		return runtimePOST("/api/docker/containers/"+url.PathEscape(flags.Arg(0))+"/resource-saver", map[string]any{"mode": *mode, "stateful": *stateful})
	default:
		return errors.New("usage: porto resource-saver status|configure|container")
	}
}

func engineResourcesCmd(args []string) error {
	if len(args) == 0 || len(args) == 1 && args[0] == "status" {
		return runtimeGET("/api/docker/engine/resources")
	}
	if len(args) == 2 && args[0] == "apply" && args[1] == "--restart-engine" {
		return runtimePOST("/api/docker/engine/resources/apply", map[string]bool{"restartEngine": true})
	}
	if args[0] != "configure" {
		return errors.New("usage: porto docker engine-resources status|configure|apply --restart-engine")
	}
	if !daemonUp() {
		return errors.New("start the Porto daemon before configuring engine budgets")
	}
	var document bytes.Buffer
	if err := api("GET", "/api/docker/engine/resources", nil, &document); err != nil {
		return err
	}
	var current portodocker.EngineResourceStatus
	if err := json.Unmarshal(document.Bytes(), &current); err != nil {
		return err
	}
	limits := current.Limits
	flags := flag.NewFlagSet("docker engine-resources configure", flag.ContinueOnError)
	flags.IntVar(&limits.CPUs, "cpus", limits.CPUs, "engine virtual CPU budget")
	flags.IntVar(&limits.MemoryMiB, "memory", limits.MemoryMiB, "engine memory budget in MiB")
	flags.IntVar(&limits.DiskGiB, "disk", limits.DiskGiB, "engine disk budget in GiB; existing disks may only grow")
	swap := flags.Int("swap", 0, "managed guest swap budget in MiB; zero disables owned swap")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: porto docker engine-resources configure [--cpus 2] [--memory 4096] [--swap 0] [--disk 100]")
	}
	flags.Visit(func(option *flag.Flag) {
		if option.Name == "swap" {
			limits.SwapMiB = swap
		}
	})
	if err := limits.Validate(); err != nil {
		return err
	}
	return api("PUT", "/api/docker/engine/resources", limits, os.Stdout)
}
