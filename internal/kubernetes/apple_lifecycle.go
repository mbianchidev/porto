package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

func (p *ClusterProvisioner) beginAppleLifecycle(request *ClusterRequest, goal string) error {
	state := request.Apple
	if state.Operation == nil || state.Operation.Goal != goal || state.Operation.CompletedAt != "" {
		id, err := appleIdentity()
		if err != nil {
			return err
		}
		state.Operation = &AppleLifecycle{
			ID: id, Goal: goal, StartedAt: time.Now().UTC().Format(time.RFC3339Nano), Nodes: make(map[string]string),
		}
	}
	if state.Operation.Nodes == nil {
		state.Operation.Nodes = make(map[string]string)
	}
	request.Phase = "starting"
	if goal == "stopped" {
		request.Phase = "stopping"
	}
	request.Error, state.Operation.Error = "", ""
	return p.writeClusterMetadata(*request)
}

func (p *ClusterProvisioner) appleProgress(request *ClusterRequest, stage, node, progress string) error {
	request.Apple.Operation.Stage = stage
	if node != "" {
		request.Apple.Operation.Nodes[node] = progress
	}
	return p.writeClusterMetadata(*request)
}

func (p *ClusterProvisioner) finishAppleLifecycle(request *ClusterRequest, failure error) error {
	operation := request.Apple.Operation
	if failure != nil {
		request.Phase = "start-failed"
		if operation.Goal == "stopped" {
			request.Phase = "stop-failed"
		}
		request.Error, operation.Error = failure.Error(), failure.Error()
		operation.Stage = "incomplete"
		return errors.Join(failure, p.writeClusterMetadata(*request))
	}
	operation.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	operation.Stage = "complete"
	request.Phase, request.Error, operation.Error = "", "", ""
	return p.writeClusterMetadata(*request)
}

func (p *ClusterProvisioner) stopAppleNative(ctx context.Context, request *ClusterRequest) (err error) {
	ctx, cancel := provisioningContext(ctx, "gracefully stop native Apple cluster", 15*time.Minute)
	defer cancel()
	containers, err := p.ownedAppleNodes(ctx, *request)
	if err != nil {
		return err
	}
	continuing := request.Apple.Operation != nil && request.Apple.Operation.Goal == "stopped" &&
		request.Apple.Operation.CompletedAt == ""
	allStopped := true
	for _, container := range containers {
		allStopped = allStopped && container.Status.State == "stopped"
	}
	if !allStopped && !continuing {
		preserveRecovery := request.Apple.Operation != nil && request.Apple.Operation.Goal == "running" &&
			request.Apple.Operation.CompletedAt == "" && request.Apple.Health != nil
		prior := request.Apple.Health
		snapshotContext, cancel := context.WithTimeout(ctx, time.Minute)
		snapshot, snapshotErr := p.captureAppleHealth(snapshotContext, *request)
		cancel()
		if snapshotErr != nil {
			return fmt.Errorf("cannot safely snapshot workload shutdown budgets before stopping any node: %w", snapshotErr)
		}
		if preserveRecovery {
			healthy := make(map[string]bool)
			for _, workload := range prior.Workloads {
				healthy[workload.UID] = workload.Healthy
			}
			for index := range snapshot.Workloads {
				if !snapshot.Workloads[index].Ignored && healthy[snapshot.Workloads[index].UID] {
					snapshot.Workloads[index].Healthy = true
				}
			}
		}
		request.Apple.Health = snapshot
	}
	if err := p.beginAppleLifecycle(request, "stopped"); err != nil {
		return err
	}
	defer func() { err = p.finishAppleLifecycle(request, err) }()
	order := make([]AppleNode, 0, len(request.Apple.Nodes))
	for _, node := range request.Apple.Nodes {
		if node.Role == "worker" {
			order = append(order, node)
		}
	}
	slices.Reverse(order)
	for _, node := range request.Apple.Nodes {
		if node.Role == "control-plane" {
			order = append(order, node)
		}
	}
	for _, node := range order {
		if err := p.appleProgress(request, "graceful-node-shutdown", node.ID, "checking"); err != nil {
			return err
		}
		if err := p.stopAppleNode(ctx, *request, node); err != nil {
			return err
		}
		if err := p.appleProgress(request, "graceful-node-shutdown", node.ID, "stopped"); err != nil {
			return err
		}
	}
	return nil
}

func (p *ClusterProvisioner) stopAppleNode(ctx context.Context, request ClusterRequest, node AppleNode) error {
	containers, err := p.ownedAppleNodes(ctx, request)
	if err != nil {
		return err
	}
	container := containers[node.ID]
	if container.Status.State == "stopped" {
		return nil
	}
	if container.Status.State != "running" {
		return fmt.Errorf("native node %s is %s; retry after its current transition settles", node.ID, container.Status.State)
	}
	if err := p.checkAppleShutdownBudget(ctx, request, node); err != nil {
		return err
	}
	const shutdown = `set -eu
systemctl is-active --quiet dbus.service
systemctl is-active --quiet systemd-logind.service
systemctl is-active --quiet kubelet.service
systemd-inhibit --list --no-pager --no-legend | grep -q Kubelet
sync
systemctl poweroff --no-block`
	shutdownContext, cancel := context.WithTimeout(ctx, appleShutdown)
	defer cancel()
	_, requestErr := p.appleGuest(shutdownContext, node.ID, 30*time.Second, nil, "/bin/sh", "-c", shutdown)
	if requestErr != nil {
		return fmt.Errorf("graceful shutdown request for %s was not acknowledged; no forced shutdown was attempted: %w", node.ID, requestErr)
	}
	return waitApple(shutdownContext, "verify native VM "+node.ID+" stopped", func(probe context.Context) error {
		current, err := p.inspectAppleNodes(probe, []string{node.ID})
		if err != nil {
			return err
		}
		if current[node.ID].Status.State != "stopped" {
			return fmt.Errorf("native VM %s is still %s; no forced shutdown will be used", node.ID, current[node.ID].Status.State)
		}
		return p.verifyAppleStorage(request.Apple.AppRoot, node)
	})
}

func (p *ClusterProvisioner) checkAppleShutdownBudget(ctx context.Context, request ClusterRequest, node AppleNode) error {
	data, err := p.appleReadFile(ctx, node.ID, "/var/lib/kubelet/config.yaml")
	if err != nil {
		return fmt.Errorf("read graceful node shutdown configuration: %w", err)
	}
	var configuration struct {
		Grace      string `json:"shutdownGracePeriod"`
		Critical   string `json:"shutdownGracePeriodCriticalPods"`
		Priorities []any  `json:"shutdownGracePeriodByPodPriority"`
	}
	if err := yaml.Unmarshal(data, &configuration); err != nil {
		return err
	}
	total, err := time.ParseDuration(configuration.Grace)
	if err != nil || total <= 0 {
		return errors.New("kubelet graceful node shutdown is not configured; refusing to power off workload data")
	}
	critical, err := time.ParseDuration(configuration.Critical)
	if err != nil || critical <= 0 || critical >= total || len(configuration.Priorities) > 0 {
		return errors.New("native node has an unverified shutdown priority budget; refusing an implicit forced workload shutdown")
	}
	required := ApplePodGrace{}
	if request.Apple.Health != nil {
		required = request.Apple.Health.Shutdown[node.ID]
	}
	if required.Regular < 0 || required.Critical < 0 || required.Regular > 30*24*60*60 || required.Critical > 30*24*60*60 {
		return errors.New("native workload shutdown grace is outside the verified bounded policy")
	}
	regularSeconds := int64((total - critical) / time.Second)
	criticalSeconds := int64(critical / time.Second)
	desiredRegular := max(regularSeconds, required.Regular+30)
	desiredCritical := max(criticalSeconds, required.Critical+30)
	if desiredRegular != regularSeconds || desiredCritical != criticalSeconds {
		var document map[string]any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return err
		}
		total = time.Duration(desiredRegular+desiredCritical) * time.Second
		critical = time.Duration(desiredCritical) * time.Second
		document["shutdownGracePeriod"] = total.String()
		document["shutdownGracePeriodCriticalPods"] = critical.String()
		encoded, err := yaml.Marshal(document)
		if err != nil {
			return err
		}
		if err := p.appleWriteFile(ctx, node.ID, "/var/lib/kubelet/config.yaml", encoded); err != nil {
			return err
		}
		logind := fmt.Sprintf("[Login]\nInhibitDelayMaxSec=%d\n", int64(total/time.Second)+60)
		if err := p.appleWriteFile(ctx, node.ID, "/etc/systemd/logind.conf.d/porto.conf", []byte(logind)); err != nil {
			return err
		}
		_, err = p.appleGuest(ctx, node.ID, time.Minute, nil, "/bin/sh", "-ec",
			"systemctl restart systemd-logind.service; systemctl restart kubelet.service")
		if err != nil {
			return fmt.Errorf("configure node shutdown to honor existing Pod termination grace: %w", err)
		}
	}
	for index := range request.Apple.Nodes {
		if request.Apple.Nodes[index].ID == node.ID {
			request.Apple.Nodes[index].ShutdownGraceSeconds = int64(total / time.Second)
			request.Apple.Nodes[index].CriticalShutdownSeconds = int64(critical / time.Second)
		}
	}
	if err := p.writeClusterMetadata(request); err != nil {
		return err
	}
	inhibitorContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return waitApple(inhibitorContext, "wait for kubelet graceful shutdown inhibitor "+node.ID, func(probe context.Context) error {
		_, err := p.appleGuest(probe, node.ID, 10*time.Second, nil, "/bin/sh", "-ec",
			"systemctl is-active --quiet kubelet.service; systemd-inhibit --list --no-pager --no-legend | grep -q Kubelet")
		return err
	})
}

func (p *ClusterProvisioner) readAppleLifecycleMetadata(name string) (ClusterRequest, error) {
	request, err := p.readClusterMetadata(name)
	if err != nil {
		return request, err
	}
	if request.Provider != "apple" || request.Apple == nil {
		return p.readMutableClusterMetadata(name)
	}
	switch request.Phase {
	case "", "starting", "stopping", "start-failed", "stop-failed":
		return request, nil
	default:
		return request, fmt.Errorf("native cluster %s is %s; creation must complete before lifecycle operations", name, request.Phase)
	}
}

func (p *ClusterProvisioner) Restart(ctx context.Context, name string) error {
	if !clusterNamePattern.MatchString(name) {
		return fmt.Errorf("cluster name must match %s", clusterNamePattern)
	}
	request, err := p.readAppleLifecycleMetadata(name)
	if err != nil {
		return err
	}
	release, err := p.reserveRuntimeName(clusterRuntimeName(request), request.Name)
	if err != nil {
		return err
	}
	defer release()
	releaseNative, err := p.lockAppleOperation(ctx, &request, "restart")
	if err != nil {
		return err
	}
	defer releaseNative()
	if err := p.setRunning(ctx, name, request, false); err != nil {
		return err
	}
	request, err = p.readAppleLifecycleMetadata(name)
	if err != nil {
		return err
	}
	return p.setRunning(ctx, name, request, true)
}

func appleNodeByRole(request *ClusterRequest, role string) *AppleNode {
	for index := range request.Apple.Nodes {
		if request.Apple.Nodes[index].Role == role {
			return &request.Apple.Nodes[index]
		}
	}
	return nil
}

func appleAppendWarning(state *AppleClusterState, warning string) {
	if warning != "" && !slices.Contains(state.Warnings, warning) {
		state.Warnings = append(state.Warnings, strings.TrimSpace(warning))
	}
}
