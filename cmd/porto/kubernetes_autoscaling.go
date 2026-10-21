package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"

	"github.com/mbianchidev/porto/internal/autoscaling"
)

func kubernetesAutoscalingCmd(args []string) error {
	fs := flag.NewFlagSet("kubernetes cluster autoscaling", flag.ContinueOnError)
	nodes := fs.Bool("nodes", false, "enable real Karpenter worker VMs; --nodes=false pauses provisioning")
	keda := fs.Bool("keda", false, "enable KEDA workload controllers")
	maxNodes := fs.Int("max-nodes", 3, "maximum autoscaled workers")
	cpus := fs.Int("cpus", 2, "CPUs per autoscaled worker")
	memory := fs.Int("memory", 2048, "memory MiB per autoscaled worker")
	disk := fs.Int("disk", 20, "disk GiB per autoscaled worker")
	if err := parseInterspersed(fs, args, map[string]bool{"nodes": true, "keda": true}); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: porto kubernetes cluster autoscaling <cluster> [--nodes=true|false] [--keda=true|false] [--max-nodes 3] [--cpus 2] [--memory 2048] [--disk 20]")
	}
	path := "/api/kubernetes/clusters/" + url.PathEscape(fs.Arg(0)) + "/autoscaling"
	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })
	if len(visited) == 0 {
		return runtimeGET(path)
	}
	if !daemonUp() {
		return errors.New("daemon is not running; start it with 'porto daemon start'")
	}
	var output bytes.Buffer
	if err := api("GET", path, nil, &output); err != nil {
		return err
	}
	var current autoscaling.Status
	if err := json.Unmarshal(output.Bytes(), &current); err != nil {
		return fmt.Errorf("decode current autoscaling configuration: %w", err)
	}
	cfg := current.Config
	if visited["nodes"] {
		cfg.Nodes.Enabled = *nodes
		if *nodes && cfg.Nodes.MaxNodes == 0 {
			cfg.Nodes.MaxNodes = *maxNodes
		}
		if *nodes {
			if cfg.Nodes.Machine.CPUs == 0 {
				cfg.Nodes.Machine.CPUs = *cpus
			}
			if cfg.Nodes.Machine.MemoryMiB == 0 {
				cfg.Nodes.Machine.MemoryMiB = *memory
			}
			if cfg.Nodes.Machine.DiskGiB == 0 {
				cfg.Nodes.Machine.DiskGiB = *disk
			}
		}
	}
	if visited["keda"] {
		cfg.KEDA = *keda
	}
	if visited["max-nodes"] {
		cfg.Nodes.MaxNodes = *maxNodes
	}
	if visited["cpus"] {
		cfg.Nodes.Machine.CPUs = *cpus
	}
	if visited["memory"] {
		cfg.Nodes.Machine.MemoryMiB = *memory
	}
	if visited["disk"] {
		cfg.Nodes.Machine.DiskGiB = *disk
	}
	return runtimePOST(path, cfg)
}

func kubernetesWorkloadScalingCmd(args []string) error {
	fs := flag.NewFlagSet("kubernetes cluster workload-scale", flag.ContinueOnError)
	file := fs.String("file", "", "JSON ScaledObject request with trigger authentication references")
	if err := parseInterspersed(fs, args, nil); err != nil {
		return err
	}
	if fs.NArg() != 1 || *file == "" {
		return errors.New("usage: porto kubernetes cluster workload-scale <cluster> --file <request.json>")
	}
	input, err := os.Open(*file)
	if err != nil {
		return fmt.Errorf("open workload scaling request: %w", err)
	}
	defer input.Close()
	decoder := json.NewDecoder(io.LimitReader(input, 2*1024*1024+1))
	decoder.DisallowUnknownFields()
	var request autoscaling.ScaledObject
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode workload scaling request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("workload scaling request must contain exactly one JSON object")
	}
	return runtimePOST("/api/kubernetes/clusters/"+url.PathEscape(fs.Arg(0))+"/autoscaling/workloads", request)
}

func kubernetesWorkloadUnscaleCmd(args []string) error {
	fs := flag.NewFlagSet("kubernetes cluster workload-unscale", flag.ContinueOnError)
	confirm := fs.Bool("confirm", false, "confirm removal of event-driven replica control")
	if err := parseInterspersed(fs, args, map[string]bool{"confirm": true}); err != nil {
		return err
	}
	if fs.NArg() != 3 || !*confirm {
		return errors.New("usage: porto kubernetes cluster workload-unscale <cluster> <namespace> <scaler> --confirm")
	}
	return runtimeDELETE("/api/kubernetes/clusters/" + url.PathEscape(fs.Arg(0)) + "/autoscaling/workloads/" + url.PathEscape(fs.Arg(1)) + "/" + url.PathEscape(fs.Arg(2)) + "?confirm=true")
}
