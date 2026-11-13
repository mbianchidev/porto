package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path"
	"runtime"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
)

type taskCleanupRequest struct {
	address   string
	namespace string
	id        string
}

func parseTaskCleanupRequest(args []string) (taskCleanupRequest, error) {
	flags := flag.NewFlagSet("task-stop-cleanup", flag.ContinueOnError)
	request := taskCleanupRequest{}
	flags.StringVar(&request.address, "address", "", "backend-local containerd socket")
	flags.StringVar(&request.namespace, "namespace", "", "containerd namespace")
	flags.StringVar(&request.id, "id", "", "full container ID")
	if err := flags.Parse(args); err != nil {
		return request, err
	}
	identity, err := hex.DecodeString(request.id)
	if !path.IsAbs(request.address) || request.namespace == "" ||
		err != nil || len(identity) != 32 || flags.NArg() != 0 {
		return request, errors.New("task-stop-cleanup requires an absolute socket, namespace, and full hexadecimal container ID")
	}
	return request, nil
}

func runTaskStopCleanup(ctx context.Context, args []string) (err error) {
	request, err := parseTaskCleanupRequest(args)
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return errors.New("task-stop-cleanup must run on the Linux backend")
	}
	client, err := containerd.New(request.address, containerd.WithDefaultNamespace(request.namespace))
	if err != nil {
		return fmt.Errorf("connect stopped-task cleanup: %w", err)
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	container, err := client.LoadContainer(ctx, request.id)
	if err != nil {
		return fmt.Errorf("load stopped-task cleanup identity: %w", err)
	}
	info, err := container.Info(ctx)
	if err != nil {
		return fmt.Errorf("verify stopped-task cleanup identity: %w", err)
	}
	if info.ID != request.id {
		return errors.New("stopped-task cleanup identity changed")
	}
	return cleanupStoppedContainer(ctx, container, stopLegacyHealthUnits)
}

func cleanupStoppedContainer(ctx context.Context, container containerd.Container, stopHealth func(context.Context, string) error) (err error) {
	task, taskErr := container.Task(ctx, cio.Load)
	if taskErr != nil && !errdefs.IsNotFound(taskErr) {
		return fmt.Errorf("load stopped task IO: %w", taskErr)
	}
	if taskErr == nil {
		if task == nil {
			return errors.New("containerd returned no task for auxiliary cleanup")
		}
		state, stateErr := task.Status(ctx)
		if stateErr != nil {
			return fmt.Errorf("verify stopped task before auxiliary cleanup: %w", stateErr)
		}
		if state.Status != containerd.Stopped {
			return fmt.Errorf("%w: auxiliary cleanup requires a stopped task", errdefs.ErrFailedPrecondition)
		}
		if stream := task.IO(); stream != nil {
			defer func() { err = errors.Join(err, stream.Close()) }()
		}
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return fmt.Errorf("read stopped-task health configuration: %w", err)
	}
	if encoded := labels["nerdctl/healthcheck"]; encoded != "" {
		var health struct{ Test []string }
		if err := json.Unmarshal([]byte(encoded), &health); err != nil {
			return fmt.Errorf("decode stopped-task health configuration: %w", err)
		}
		if len(health.Test) > 0 && health.Test[0] != "NONE" {
			return stopHealth(ctx, container.ID())
		}
	}
	return nil
}
