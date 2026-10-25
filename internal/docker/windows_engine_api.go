package docker

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/mbianchidev/porto/internal/containerlogs"
	"github.com/mbianchidev/porto/internal/runtimes"
	"github.com/mbianchidev/porto/internal/windowscontainers"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func randomWindowsContainerID() (string, error) {
	value := make([]byte, 32)
	_, err := rand.Read(value)
	return hex.EncodeToString(value), err
}
func marshalWindowsSpec(spec *specs.Spec) (typeurl.Any, error) { return typeurl.MarshalAny(spec) }
func killWindowsProcess(ctx context.Context, process containerd.Process) error {
	err := process.Kill(windowsContext(ctx), syscall.SIGKILL)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (e *windowsEngine) Inspect(ctx context.Context, id string) (json.RawMessage, error) {
	container, request, err := e.find(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx = windowsContext(ctx)
	record, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("read native container %s OCI spec: %w", container.ID(), err)
	}
	if spec == nil || spec.Process == nil {
		return nil, fmt.Errorf("native container %s OCI spec has no process", container.ID())
	}
	labels := record.Labels
	state := labels[windowsStateLabel]
	if state == "" {
		state = "created"
	}
	exitCode, err := persistedWindowsExitCode(container.ID(), labels)
	if err != nil {
		return nil, err
	}
	code := uint64(exitCode)
	pid := uint32(0)
	if task, err := container.Task(ctx, nil); err == nil {
		status, err := task.Status(ctx)
		if err != nil {
			return nil, err
		}
		pid = task.Pid()
		switch status.Status {
		case containerd.Running:
			state = "running"
		case containerd.Paused, containerd.Pausing:
			state = "paused"
		case containerd.Stopped:
			state = "exited"
			code = uint64(status.ExitStatus)
			pid = 0
		case containerd.Created:
			state = "created"
		default:
			return nil, fmt.Errorf("%w: native task status cannot be determined", ErrUnavailable)
		}
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}
	attachment, err := persistedWindowsAttachment(container.ID(), labels)
	if err != nil {
		return nil, err
	}
	ports := map[string][]map[string]string{}
	mappings, err := windowscontainers.ParsePublishedPorts(request.Publish)
	if err != nil {
		return nil, err
	}
	for _, port := range mappings {
		key := fmt.Sprintf("%d/%s", port.ContainerPort, port.Protocol)
		ports[key] = append(ports[key], map[string]string{"HostIp": port.HostIP, "HostPort": strconv.Itoa(int(port.HostPort))})
	}
	networks := map[string]any{}
	if attachment.Network != "" && attachment.Network != "none" {
		networks[attachment.Network] = map[string]any{"NetworkID": attachment.NetworkID, "EndpointID": strings.Join(attachment.Endpoints, ","),
			"IPAddress": attachment.Address, "Gateway": attachment.Gateway}
	}
	mounts := []map[string]any{}
	for _, mount := range spec.Mounts {
		readonly := false
		for _, option := range mount.Options {
			if option == "ro" {
				readonly = true
			}
		}
		mounts = append(mounts, map[string]any{"Type": "bind", "Source": mount.Source, "Destination": mount.Destination, "RW": !readonly})
	}
	path, args := "", []string{}
	if spec.Process != nil && len(spec.Process.Args) > 0 {
		path, args = spec.Process.Args[0], spec.Process.Args[1:]
	}
	return json.Marshal(map[string]any{
		"Id": container.ID(), "Name": "/" + labels[nerdctlNameLabel], "Created": record.CreatedAt.UTC().Format(time.RFC3339Nano),
		"Image": labels[windowsImageConfigLabel], "Path": path, "Args": args,
		"State": map[string]any{"Status": state, "Running": state == "running", "Paused": state == "paused", "Restarting": false,
			"Dead": false, "Pid": pid, "ExitCode": code, "Error": "", "StartedAt": labels["io.porto.windows.started"], "FinishedAt": labels["io.porto.windows.finished"]},
		"Config": map[string]any{"Image": request.Image, "Cmd": request.Command, "Entrypoint": request.Entrypoint, "Env": spec.Process.Env,
			"User": spec.Process.User.Username, "WorkingDir": spec.Process.Cwd, "Tty": request.TTY, "OpenStdin": request.Interactive,
			"StdinOnce": request.StdinOnce, "Labels": labels},
		"HostConfig": map[string]any{"Isolation": request.Isolation, "Runtime": windowscontainers.RuntimeName,
			"NetworkMode": request.Networks[0].Name, "PortBindings": ports, "AutoRemove": request.Remove, "Binds": request.Volumes,
			"RestartPolicy": map[string]any{"Name": "no", "MaximumRetryCount": 0}},
		"Mounts": mounts, "NetworkSettings": map[string]any{"Ports": ports, "Networks": networks},
		"LogPath": labels[portoLogPathLabel], "Driver": windowscontainers.Snapshotter, "Platform": "windows",
	})
}

func (e *windowsEngine) Containers(ctx context.Context, all bool) ([]Container, error) {
	containers, err := e.store.List(windowsContext(ctx))
	if err != nil {
		return nil, err
	}
	result := make([]Container, 0, len(containers))
	for _, container := range containers {
		record, err := container.Info(windowsContext(ctx))
		if err != nil {
			return nil, err
		}
		if record.Labels[windowsOwnerLabel] != e.manager.windows.installation.OwnerSID {
			continue
		}
		document, err := e.Inspect(ctx, container.ID())
		if err != nil {
			return nil, err
		}
		var inspected struct {
			State struct {
				Status   string
				Running  bool
				Pid      uint32
				ExitCode uint32
			}
			Image string
		}
		if err := json.Unmarshal(document, &inspected); err != nil {
			return nil, err
		}
		if !all && !inspected.State.Running {
			continue
		}
		request, err := persistedWindowsRequest(container.ID(), record.Labels)
		if err != nil {
			return nil, err
		}
		containerValue := Container{ID: container.ID(), Name: record.Labels[nerdctlNameLabel], Image: request.Image, Labels: record.Labels,
			CreatedAt: record.CreatedAt.Format(time.RFC3339Nano), Health: ContainerHealth{Status: "none"}}
		spec, err := container.Spec(windowsContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("read native container %s OCI spec for inventory: %w", container.ID(), err)
		}
		if spec == nil || spec.Process == nil {
			return nil, fmt.Errorf("native container %s OCI spec has no process", container.ID())
		}
		mapContainerSpec(&containerValue, spec)
		if err := applyWindowsContainerMetadata(&containerValue, record.Labels, inspected.State.Running); err != nil {
			return nil, err
		}
		containerValue.State, containerValue.Status = inspected.State.Status, inspected.State.Status
		containerValue.PID = inspected.State.Pid
		containerValue.TaskPresent = containerValue.State == "running" || containerValue.State == "paused"
		containerValue.ImageID = inspected.Image
		exit := inspected.State.ExitCode
		containerValue.ExitCode = &exit
		result = append(result, containerValue)
	}
	return result, nil
}
func applyWindowsContainerMetadata(container *Container, labels map[string]string, taskPresent bool) error {
	if labels[windowsRequestLabel] == "" && labels[windowsOwnerLabel] == "" {
		return nil
	}
	request, err := persistedWindowsRequest(container.ID, labels)
	if err != nil {
		return err
	}
	attachment, err := persistedWindowsAttachment(container.ID, labels)
	if err != nil {
		return err
	}
	code, err := persistedWindowsExitCode(container.ID, labels)
	if err != nil {
		return err
	}
	container.Image = request.Image
	container.ImageID = labels[windowsImageConfigLabel]
	container.Labels = labels
	if len(request.Networks) > 0 {
		container.Networks = request.Networks[0].Name
	}
	if attachment.Network != "" {
		container.NetworkDetails = []ContainerNetworkState{{Name: attachment.Network, IPAddress: attachment.Address, Gateway: attachment.Gateway}}
	}
	mappings, err := windowscontainers.ParsePublishedPorts(request.Publish)
	if err != nil {
		return fmt.Errorf("native container %s published port metadata: %w", container.ID, err)
	}
	var ports []string
	for _, port := range mappings {
		container.NetworkDetails = append(container.NetworkDetails, ContainerNetworkState{Name: attachment.Network, HostIP: port.HostIP,
			HostPort: port.HostPort, ContainerPort: port.ContainerPort, Protocol: port.Protocol, IPAddress: attachment.Address})
		ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", port.HostIP, port.HostPort, port.ContainerPort, port.Protocol))
	}
	container.Ports = strings.Join(ports, ", ")
	for i := range container.MountDetails {
		if container.MountDetails[i].Type == "" {
			container.MountDetails[i].Type = "bind"
		}
	}
	if !taskPresent && labels[windowsStateLabel] == "exited" {
		exit := code
		container.State, container.Status = "exited", fmt.Sprintf("Exited (%d)", code)
		container.ExitCode = &exit
		container.ExitAt = labels["io.porto.windows.finished"]
		container.PID = 0
	}
	return nil
}
func (e *windowsEngine) Exec(ctx context.Context, request ExecRequest) (runtimes.Process, error) {
	container, _, err := e.find(ctx, request.ContainerID)
	if err != nil {
		return nil, err
	}
	task, err := container.Task(windowsContext(ctx), nil)
	if err != nil {
		return nil, err
	}
	status, err := task.Status(windowsContext(ctx))
	if err != nil {
		return nil, err
	}
	if status.Status != containerd.Running {
		return nil, fmt.Errorf("%w: native container is not running", ErrConflict)
	}
	spec, err := container.Spec(windowsContext(ctx))
	if err != nil {
		return nil, err
	}
	if spec == nil || spec.Windows == nil || spec.Linux != nil || spec.Process == nil || spec.Annotations["microsoft.com/hostprocess-container"] == "true" {
		return nil, windowscontainers.ErrOwnership
	}
	encoded, err := json.Marshal(spec.Process)
	if err != nil {
		return nil, fmt.Errorf("clone native exec process: %w", err)
	}
	var processSpec specs.Process
	if err := json.Unmarshal(encoded, &processSpec); err != nil {
		return nil, err
	}
	processSpec.Args = append([]string(nil), request.Command...)
	processSpec.CommandLine = ""
	processSpec.Terminal = request.TTY
	processSpec.Env, err = windowsEnvironment(processSpec.Env, request.Environment)
	if err != nil {
		return nil, err
	}
	if request.WorkingDir != "" {
		processSpec.Cwd = request.WorkingDir
	}
	if request.User != "" {
		processSpec.User.Username = request.User
	}
	if request.TTY {
		height, width := windowsConsoleSize(request.ConsoleSize)
		processSpec.ConsoleSize = &specs.Box{Height: uint(height), Width: uint(width)}
	} else {
		processSpec.ConsoleSize = nil
	}
	id, err := randomWindowsContainerID()
	if err != nil {
		return nil, err
	}
	hub, err := e.newIO(id, request.TTY, request.AttachStdin, "")
	if err != nil {
		return nil, err
	}
	subscription := hub.subscribe(ctx, true)
	process, err := task.Exec(windowsContext(ctx), id, &processSpec, hub.creator())
	if err != nil {
		subscription.cancel()
		return nil, errors.Join(err, closeWindowsTaskIO(hub))
	}
	wait, err := process.Wait(windowsContext(context.Background()))
	if err != nil {
		subscription.cancel()
		deleteErr := deleteWindowsTask(ctx, process)
		if deleteErr != nil {
			return nil, errors.Join(err, deleteErr)
		}
		return nil, errors.Join(err, closeWindowsTaskIO(hub))
	}
	session := &windowsSession{process: process, hub: hub, done: make(chan struct{})}
	if err := process.Start(windowsContext(ctx)); err != nil {
		subscription.cancel()
		deleteErr := deleteWindowsTask(ctx, process)
		if deleteErr != nil {
			go e.watch(container, session, wait, windowscontainers.Attachment{}, false)
			return nil, errors.Join(err, deleteErr)
		}
		return nil, errors.Join(err, closeWindowsTaskIO(hub))
	}
	go e.watch(container, session, wait, windowscontainers.Attachment{}, false)
	result := &windowsSessionProcess{session: session, subscription: subscription, ctx: ctx, containerID: container.ID(), exec: true, input: request.AttachStdin}
	go func() {
		select {
		case <-ctx.Done():
			_ = result.Kill()
		case <-session.done:
		}
	}()
	return result, nil
}

func (e *windowsEngine) Attach(ctx context.Context, id string, stdin, start bool, size [2]uint) (runtimes.Process, error) {
	container, request, err := e.find(ctx, id)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	session := e.sessions[container.ID()]
	e.mu.Unlock()
	if start {
		if session != nil {
			return nil, fmt.Errorf("%w: native container is already running", ErrConflict)
		}
		session, err = e.start(ctx, container, request)
		if err != nil {
			return nil, err
		}
	}
	if session == nil {
		return nil, fmt.Errorf("%w: native task I/O is not attached to this worker; recover/stop the owned task explicitly", ErrUnavailable)
	}
	if request.TTY && size != [2]uint{} {
		height, width := windowsConsoleSize(size)
		if err := session.process.Resize(windowsContext(ctx), width, height); err != nil {
			return nil, err
		}
	}
	subscription := session.hub.subscribe(ctx, start)
	return &windowsSessionProcess{session: session, subscription: subscription, ctx: ctx, containerID: container.ID(), input: stdin}, nil
}

func (e *windowsEngine) Logs(ctx context.Context, id string, options LogOptions, emit func(runtimes.OutputChunk) error) error {
	container, _, err := e.find(ctx, id)
	if err != nil {
		return err
	}
	path := filepath.Join(e.manager.stateDir, "container-logs", container.ID()+".log.jsonl")
	tail, err := parseLogTail(options.Tail)
	if err != nil {
		return err
	}
	since, _, err := parseDockerEventTime(options.Since)
	if err != nil {
		return err
	}
	until, _, err := parseDockerEventTime(options.Until)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, containerlogs.MaxLineBytes*8)
	var records []containerlogs.Record
	var pending []byte
	read := func() error {
		for {
			line, readErr := reader.ReadBytes('\n')
			pending = append(pending, line...)
			if len(pending) > containerlogs.MaxLineBytes*8 {
				return errors.New("native log record exceeds bounded size")
			}
			if len(line) > 0 && line[len(line)-1] == '\n' {
				var record containerlogs.Record
				if err := json.Unmarshal(pending, &record); err != nil {
					return err
				}
				records = append(records, record)
				pending = nil
			}
			if readErr != nil {
				if readErr == io.EOF {
					return nil
				}
				return readErr
			}
		}
	}
	if err := read(); err != nil {
		return err
	}
	if tail >= 0 && len(records) > tail {
		records = records[len(records)-tail:]
	}
	deliver := func() error {
		for _, record := range records {
			if record.Stream == "stdout" && !options.Stdout || record.Stream == "stderr" && !options.Stderr {
				continue
			}
			if !since.IsZero() && record.Timestamp.Before(since) || !until.IsZero() && record.Timestamp.After(until) {
				continue
			}
			text := record.Text
			if options.Timestamps {
				text = record.Timestamp.Format(time.RFC3339Nano) + " " + text
			}
			if err := emit(runtimes.OutputChunk{Stream: record.Stream, Data: []byte(text)}); err != nil {
				return err
			}
		}
		records = nil
		return nil
	}
	if err := deliver(); err != nil {
		return err
	}
	if !options.Follow {
		return nil
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		e.mu.Lock()
		session := e.sessions[container.ID()]
		e.mu.Unlock()
		if session == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-session.done:
			if err := read(); err != nil {
				return err
			}
			return deliver()
		case <-ticker.C:
			if err := read(); err != nil {
				return err
			}
			if err := deliver(); err != nil {
				return err
			}
		}
	}
}
