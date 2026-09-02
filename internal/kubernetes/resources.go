package kubernetes

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	resourceusage "github.com/mbianchidev/porto/internal/resources"
)

type NodeResourceStats struct {
	Name  string              `json:"name"`
	Usage resourceusage.Usage `json:"usage"`
}

type PodResourceStats struct {
	Namespace string              `json:"namespace"`
	Pod       string              `json:"pod"`
	Container string              `json:"container"`
	Usage     resourceusage.Usage `json:"usage"`
}

type ResourceStats struct {
	Nodes []NodeResourceStats `json:"nodes"`
	Pods  []PodResourceStats  `json:"pods"`
	Total resourceusage.Usage `json:"total"`
}

func (m *Manager) ResourceStats(ctx context.Context, contextName string) (ResourceStats, error) {
	nodeOutput, err := m.run(ctx, contextName, m.timeout, nil, "top", "nodes", "--no-headers")
	if err != nil {
		if metricsError(err) {
			return ResourceStats{}, fmt.Errorf("%w: %v", ErrMetricsUnavailable, err)
		}
		return ResourceStats{}, err
	}
	podOutput, err := m.run(
		ctx,
		contextName,
		m.timeout,
		nil,
		"top",
		"pods",
		"--all-namespaces",
		"--containers",
		"--no-headers",
	)
	if err != nil {
		if metricsError(err) {
			return ResourceStats{}, fmt.Errorf("%w: %v", ErrMetricsUnavailable, err)
		}
		return ResourceStats{}, err
	}
	nodes, total, err := parseNodeResourceStats(nodeOutput)
	if err != nil {
		return ResourceStats{}, err
	}
	pods, err := parsePodResourceStats(podOutput)
	if err != nil {
		return ResourceStats{}, err
	}
	return ResourceStats{Nodes: nodes, Pods: pods, Total: total}, nil
}

func parseNodeResourceStats(output []byte) ([]NodeResourceStats, resourceusage.Usage, error) {
	nodes := make([]NodeResourceStats, 0)
	var total resourceusage.Usage
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		cpu, err := resourceusage.ParseCPU(fields[1])
		if err != nil {
			return nil, resourceusage.Usage{}, fmt.Errorf("decode Kubernetes node %s CPU: %w", fields[0], err)
		}
		memory, err := resourceusage.ParseBytes(fields[3])
		if err != nil {
			return nil, resourceusage.Usage{}, fmt.Errorf("decode Kubernetes node %s memory: %w", fields[0], err)
		}
		usage := resourceusage.Usage{CPUMillicores: cpu, MemoryBytes: memory}
		nodes = append(nodes, NodeResourceStats{Name: fields[0], Usage: usage})
		total.Add(usage)
	}
	return nodes, total, scanner.Err()
}

func parsePodResourceStats(output []byte) ([]PodResourceStats, error) {
	pods := make([]PodResourceStats, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		cpu, err := resourceusage.ParseCPU(fields[3])
		if err != nil {
			return nil, fmt.Errorf("decode Kubernetes pod %s/%s CPU: %w", fields[0], fields[1], err)
		}
		memory, err := resourceusage.ParseBytes(fields[4])
		if err != nil {
			return nil, fmt.Errorf("decode Kubernetes pod %s/%s memory: %w", fields[0], fields[1], err)
		}
		pods = append(pods, PodResourceStats{
			Namespace: fields[0],
			Pod:       fields[1],
			Container: fields[2],
			Usage:     resourceusage.Usage{CPUMillicores: cpu, MemoryBytes: memory},
		})
	}
	return pods, scanner.Err()
}

func metricsError(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "metrics api not available") ||
		strings.Contains(message, "the server could not find the requested resource") ||
		(strings.Contains(message, "metrics.k8s.io") &&
			(strings.Contains(message, "serviceunavailable") ||
				strings.Contains(message, "unable to handle the request")))
}
