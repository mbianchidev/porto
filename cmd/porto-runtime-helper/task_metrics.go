package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/mbianchidev/porto/internal/resources"
)

func writeTaskMetrics(ctx context.Context, args []string, output io.Writer) (err error) {
	if runtime.GOOS != "linux" {
		return errors.New("task metrics must be collected on the Linux backend")
	}
	flags := flag.NewFlagSet("task-metrics", flag.ContinueOnError)
	address := flags.String("address", "", "backend-local containerd socket")
	namespace := flags.String("namespace", "", "containerd namespace")
	id := flags.String("id", "", "container ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*address) || *namespace == "" || *id == "" || flags.NArg() != 0 {
		return errors.New("task-metrics requires an absolute socket, namespace, and container ID")
	}
	client, err := containerd.New(*address, containerd.WithDefaultNamespace(*namespace))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	response, err := client.TaskService().Metrics(ctx, &tasksapi.MetricsRequest{Filters: []string{"id==" + *id}})
	if err != nil {
		return fmt.Errorf("read backend task metrics: %w", err)
	}
	hostCPU, err := resources.ReadHostCPUUsage()
	if err != nil {
		return err
	}
	for _, metric := range response.GetMetrics() {
		if metric.GetID() == *id && metric.GetData() != nil {
			return json.NewEncoder(output).Encode(map[string]any{
				"metric": metric, "hostCPU": hostCPU, "read": time.Now().UTC(),
			})
		}
	}
	return fmt.Errorf("backend did not return metrics for container %s", *id)
}
