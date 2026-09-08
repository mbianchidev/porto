package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

const helperVersion = "1"

type cniRequest struct {
	Network   string
	Container string
	NetNS     string
	Aliases   []string
}

type runtimeProbe struct {
	CNI        bool   `json:"cni"`
	CNIReason  string `json:"cniReason,omitempty"`
	CRIU       bool   `json:"criu"`
	CRIUReason string `json:"criuReason,omitempty"`
}

type cniEndpointResult struct {
	Interface string   `json:"interface,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	Gateways  []string `json:"gateways,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("runtime helper command is required"))
	}
	switch os.Args[1] {
	case "version":
		fmt.Println(helperVersion)
	case "probe":
		if err := json.NewEncoder(os.Stdout).Encode(probeRuntime()); err != nil {
			fail(err)
		}
	case "cni-connect", "cni-disconnect":
		request, err := parseCNIRequest(os.Args[1], os.Args[2:])
		if err != nil {
			fail(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var result any
		if os.Args[1] == "cni-connect" {
			result, err = connectCNI(ctx, request)
		} else {
			err = disconnectCNI(ctx, request)
			result = map[string]bool{"removed": err == nil}
		}
		if err != nil {
			fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fail(fmt.Errorf("encode helper result: %w", err))
		}
	default:
		fail(fmt.Errorf("unknown runtime helper command %q", os.Args[1]))
	}
}

func parseCNIRequest(command string, args []string) (cniRequest, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	network := flags.String("network", "", "CNI network name")
	container := flags.String("container", "", "container ID")
	netns := flags.String("netns", "", "network namespace path")
	aliases := flags.String("aliases", "", "comma-separated network aliases")
	if err := flags.Parse(args); err != nil {
		return cniRequest{}, err
	}
	request := cniRequest{
		Network:   strings.TrimSpace(*network),
		Container: strings.TrimSpace(*container),
		NetNS:     strings.TrimSpace(*netns),
	}
	for _, alias := range strings.Split(*aliases, ",") {
		if alias = strings.TrimSpace(alias); alias != "" {
			request.Aliases = append(request.Aliases, alias)
		}
	}
	if request.Network == "" || request.Container == "" {
		return cniRequest{}, errors.New("network and container are required")
	}
	if command == "cni-connect" && request.NetNS == "" {
		return cniRequest{}, errors.New("network namespace is required for CNI connect")
	}
	if len(request.Aliases) > 1 {
		return cniRequest{}, errors.New("the active CNI integration supports one DNS alias per endpoint")
	}
	return request, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
