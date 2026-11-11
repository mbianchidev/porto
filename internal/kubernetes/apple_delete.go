package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (p *ClusterProvisioner) deleteAppleManaged(ctx context.Context, request ClusterRequest) error {
	if err := p.reconcileAppleDeletions(ctx, &request); err != nil {
		return err
	}
	var nodes []AppleNode
	allDeleted := true
	for _, node := range request.Apple.Nodes {
		allDeleted = allDeleted && node.Deleted
		if node.CreatedAt != "" && !node.Deleted {
			nodes = append(nodes, node)
		}
	}
	if allDeleted {
		return nil
	}
	if len(nodes) == 0 {
		output, err := p.runApple(ctx, runtimes.Command{
			Args: []string{"list", "--all", "--format", "json"}, StdoutOnly: true,
		})
		if err != nil {
			return runtimes.CommandError("verify native creation left no resources", output, err)
		}
		var containers []appleNativeContainer
		if err := json.Unmarshal(output, &containers); err != nil || containers == nil {
			return errors.Join(errors.New("native creation inventory is unavailable"), err)
		}
		for _, container := range containers {
			for _, planned := range request.Apple.Nodes {
				if container.ID == planned.ID {
					return fmt.Errorf("native node %s exists without a recorded backing identity; inspect it explicitly before removing the failed creation record", planned.ID)
				}
			}
		}
		return nil
	}
	state := *request.Apple
	state.Nodes = nodes
	owned := request
	owned.Apple = &state
	containers, err := p.ownedAppleNodesForAction(ctx, owned, true)
	if err != nil {
		return err
	}
	workloadShutdown := request.Apple.CAHash != "" ||
		request.Phase != "creating" && request.Phase != "error" && request.Phase != "deleting"
	if workloadShutdown && len(nodes) == len(request.Apple.Nodes) {
		if err := p.stopAppleNative(ctx, &request); err != nil {
			return fmt.Errorf("delete refused before data removal because graceful shutdown was incomplete: %w", err)
		}
	} else {
		for index := len(nodes) - 1; index >= 0; index-- {
			node := nodes[index]
			if containers[node.ID].Status.State == "stopped" {
				continue
			}
			if workloadShutdown {
				if err := p.stopAppleNode(ctx, request, node); err != nil {
					return fmt.Errorf("delete refused before data removal because graceful shutdown was incomplete: %w", err)
				}
				continue
			}
			// Pre-bootstrap creation has no registered user workload surface.
			_, err := p.appleGuest(ctx, node.ID, 30*time.Second, nil, "/bin/sh", "-ec", "sync; systemctl poweroff --no-block")
			if err != nil {
				return fmt.Errorf("gracefully shut down failed native creation %s: %w", node.ID, err)
			}
			shutdown, cancel := context.WithTimeout(ctx, appleShutdown)
			err = waitApple(shutdown, "stop failed native creation "+node.ID, func(probe context.Context) error {
				current, err := p.inspectAppleNodes(probe, []string{node.ID})
				if err != nil {
					return err
				}
				if current[node.ID].Status.State != "stopped" {
					return errors.New("native VM is still running; no forced shutdown is permitted")
				}
				return nil
			})
			cancel()
			if err != nil {
				return err
			}
		}
	}
	request.Phase = "deleting"
	if err := p.writeClusterMetadata(request); err != nil {
		return err
	}
	for len(state.Nodes) > 0 {
		if _, err := p.ownedAppleNodesForAction(ctx, owned, true); err != nil {
			return err
		}
		node := state.Nodes[len(state.Nodes)-1]
		for index := range request.Apple.Nodes {
			if request.Apple.Nodes[index].ID == node.ID {
				request.Apple.Nodes[index].Deleting = true
			}
		}
		if err := p.writeClusterMetadata(request); err != nil {
			return fmt.Errorf("save native deletion intent before changing backing data: %w", err)
		}
		output, err := p.runApple(ctx, runtimes.Command{Args: []string{"delete", node.ID}})
		if err != nil {
			return runtimes.CommandError("delete exactly owned stopped native node "+node.ID, output, err)
		}
		if err := p.reconcileAppleDeletions(ctx, &request); err != nil {
			return err
		}
		for _, current := range request.Apple.Nodes {
			if current.ID == node.ID && !current.Deleted {
				return fmt.Errorf("native node %s still exists after deletion; retained intent allows a safe retry", node.ID)
			}
		}
		state.Nodes = state.Nodes[:len(state.Nodes)-1]
	}
	return nil
}

func (p *ClusterProvisioner) reconcileAppleDeletions(ctx context.Context, request *ClusterRequest) error {
	pending := false
	for _, node := range request.Apple.Nodes {
		pending = pending || node.Deleting && !node.Deleted
	}
	if !pending {
		return nil
	}
	output, err := p.runApple(ctx, runtimes.Command{
		Args: []string{"list", "--all", "--format", "json"}, StdoutOnly: true,
	})
	if err != nil {
		return runtimes.CommandError("reconcile recorded native deletion intent", output, err)
	}
	var containers []appleNativeContainer
	if err := json.Unmarshal(output, &containers); err != nil || containers == nil {
		return errors.Join(errors.New("native deletion inventory is unavailable"), err)
	}
	present := make(map[string]bool, len(containers))
	for _, container := range containers {
		if container.ID == "" || present[container.ID] {
			return errors.New("native deletion inventory contains an invalid or duplicate identity")
		}
		present[container.ID] = true
	}
	changed := false
	for index := range request.Apple.Nodes {
		node := &request.Apple.Nodes[index]
		if !node.Deleting || node.Deleted || present[node.ID] {
			continue
		}
		directory := filepath.Join(request.Apple.AppRoot, "containers", node.ID)
		if node.Storage.Directory != directory || node.Storage.DirectoryID == "" {
			return fmt.Errorf("native node %s deletion has no exact recorded backing directory", node.ID)
		}
		if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("native node %s is absent but backing storage remains or cannot be verified; inspect it before retrying deletion", node.ID), err)
		}
		node.Deleted, node.Deleting, changed = true, false, true
	}
	if changed {
		if err := p.writeClusterMetadata(*request); err != nil {
			return fmt.Errorf("native deletion completed but progress could not be saved; retained intent permits reconciliation: %w", err)
		}
	}
	return nil
}
