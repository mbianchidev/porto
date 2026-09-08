package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
)

const (
	checkpointSourceIDLabel     = "io.porto.checkpoint.source-id"
	checkpointSourceLabelsLabel = "io.porto.checkpoint.source-labels"
)

func (r *grpcContainerRuntime) checkpointContainer(
	ctx context.Context,
	id,
	name string,
) ([]string, error) {
	namespacedContext := withContainerdNamespace(ctx, r.namespace)
	container, err := r.client.LoadContainer(namespacedContext, id)
	if err != nil {
		return nil, containerdOperationError("load metadata for checkpointing", id, err)
	}
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("porto.local/checkpoints/%s:%d", id, time.Now().UTC().UnixNano())
	}
	checkpoint, err := container.Checkpoint(
		namespacedContext,
		name,
		containerd.WithCheckpointTask,
		containerd.WithCheckpointImage,
		containerd.WithCheckpointRuntime,
		containerd.WithCheckpointRW,
	)
	if err != nil {
		return nil, containerdOperationError("checkpoint", id, err)
	}
	labels, err := container.Labels(namespacedContext)
	if err != nil {
		return nil, fmt.Errorf("read source labels for checkpoint %q: %w", name, err)
	}
	encodedLabels, err := json.Marshal(labels)
	if err != nil {
		return nil, fmt.Errorf("encode source labels for checkpoint %q: %w", name, err)
	}
	metadata := checkpoint.Metadata()
	if metadata.Labels == nil {
		metadata.Labels = make(map[string]string)
	}
	metadata.Labels[checkpointSourceIDLabel] = id
	metadata.Labels[checkpointSourceLabelsLabel] = string(encodedLabels)
	if _, err := r.client.ImageService().Update(namespacedContext, metadata, "labels"); err != nil {
		return nil, fmt.Errorf("persist checkpoint %q metadata: %w", name, err)
	}
	target := checkpoint.Target()
	return []string{name, target.MediaType, target.Digest.String()}, nil
}

func checkpointContainerLabels(labels map[string]string) (map[string]string, error) {
	encoded := labels[checkpointSourceLabelsLabel]
	if encoded == "" {
		return nil, nil
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		return nil, fmt.Errorf("decode checkpoint source labels: %w", err)
	}
	delete(result, restartCountLabel)
	delete(result, restartStatusLabel)
	return result, nil
}
