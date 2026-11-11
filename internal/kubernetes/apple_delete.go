package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mbianchidev/porto/internal/runtimes"
)

func (p *ClusterProvisioner) deleteAppleManaged(ctx context.Context, request ClusterRequest) error {
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
	if request.Phase == "" && len(nodes) == len(request.Apple.Nodes) {
		if err := p.stopAppleNative(ctx, &request); err != nil {
			return fmt.Errorf("delete refused before data removal because graceful shutdown was incomplete: %w", err)
		}
	} else {
		// Failed creation has no registered user workload surface. Deletion is explicit.
		for index := len(nodes) - 1; index >= 0; index-- {
			node := nodes[index]
			if containers[node.ID].Status.State == "stopped" {
				continue
			}
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
		output, err := p.runApple(ctx, runtimes.Command{Args: []string{"delete", node.ID}})
		if err != nil {
			return runtimes.CommandError("delete exactly owned stopped native node "+node.ID, output, err)
		}
		for index := range request.Apple.Nodes {
			if request.Apple.Nodes[index].ID == node.ID {
				request.Apple.Nodes[index].Deleted = true
			}
		}
		if err := p.writeClusterMetadata(request); err != nil {
			return fmt.Errorf("native node was deleted but deletion progress could not be saved: %w", err)
		}
		state.Nodes = state.Nodes[:len(state.Nodes)-1]
	}
	return nil
}
