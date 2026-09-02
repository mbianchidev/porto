package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultInventoryDebounce          = 100 * time.Millisecond
	defaultInventoryReconcileInterval = 30 * time.Second
	defaultInventoryConnectBackoff    = 250 * time.Millisecond
	defaultInventoryMaxBackoff        = 30 * time.Second
	defaultInventoryOperationTimeout  = 20 * time.Second
	maxInventoryEvents                = 200
	maxContainerEvents                = 20
)

type containerRuntime interface {
	Namespace() string
	Backend() string
	Snapshot(context.Context) ([]Container, error)
	Subscribe(context.Context) (<-chan ContainerLifecycleEvent, <-chan error)
	Close() error
}

type containerRuntimeConnector func(context.Context) (containerRuntime, error)

type inventoryOptions struct {
	debounce          time.Duration
	reconcileInterval time.Duration
	connectBackoff    time.Duration
	maxBackoff        time.Duration
	operationTimeout  time.Duration
}

func defaultInventoryOptions() inventoryOptions {
	return inventoryOptions{
		debounce:          defaultInventoryDebounce,
		reconcileInterval: defaultInventoryReconcileInterval,
		connectBackoff:    defaultInventoryConnectBackoff,
		maxBackoff:        defaultInventoryMaxBackoff,
		operationTimeout:  defaultInventoryOperationTimeout,
	}
}

type containerInventory struct {
	connector containerRuntimeConnector
	options   inventoryOptions

	mu          sync.RWMutex
	snapshot    ContainerSnapshot
	sequence    uint64
	subscribers map[uint64]chan ContainerSnapshot
	nextID      uint64
	refresh     chan struct{}
	instanceID  string
}

func newContainerInventory(connector containerRuntimeConnector, options inventoryOptions) *containerInventory {
	if options.debounce <= 0 {
		options.debounce = defaultInventoryDebounce
	}
	if options.reconcileInterval <= 0 {
		options.reconcileInterval = defaultInventoryReconcileInterval
	}
	if options.connectBackoff <= 0 {
		options.connectBackoff = defaultInventoryConnectBackoff
	}
	if options.maxBackoff < options.connectBackoff {
		options.maxBackoff = defaultInventoryMaxBackoff
	}
	if options.operationTimeout <= 0 {
		options.operationTimeout = defaultInventoryOperationTimeout
	}
	inventory := &containerInventory{
		connector:   connector,
		options:     options,
		subscribers: map[uint64]chan ContainerSnapshot{},
		refresh:     make(chan struct{}, 1),
		instanceID:  newInventoryInstanceID(),
		snapshot: ContainerSnapshot{
			Message:      "Connecting to containerd",
			Containers:   []Container{},
			Capabilities: containerCapabilities(),
		},
	}
	inventory.snapshot.InstanceID = inventory.instanceID
	return inventory
}

func newInventoryInstanceID() string {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func containerCapabilities() ContainerCapabilities {
	return ContainerCapabilities{
		DirectInventory: RuntimeCapability{Supported: true},
		LifecycleEvents: RuntimeCapability{Supported: true},
		CheckpointRestore: RuntimeCapability{
			Supported: false,
			Reason:    "Porto does not expose container checkpoint and restore operations yet",
		},
	}
}

func (i *containerInventory) run(ctx context.Context) {
	defer i.closeSubscribers()
	backoff := i.options.connectBackoff
	for {
		if ctx.Err() != nil {
			i.markUnavailable(errors.New("container inventory stopped"))
			return
		}
		runtimeClient, err := i.connector(ctx)
		if err != nil {
			i.markUnavailable(fmt.Errorf("connect container inventory: %w", err))
			log.Printf("container inventory connection: %v", err)
			if !waitForInventoryRetry(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, i.options.maxBackoff)
			continue
		}

		backoff = i.options.connectBackoff
		err = i.runConnected(ctx, runtimeClient)
		closeErr := runtimeClient.Close()
		if ctx.Err() != nil {
			i.markUnavailable(errors.New("container inventory stopped"))
			return
		}
		err = errors.Join(err, closeErr)
		i.markUnavailable(fmt.Errorf("container inventory disconnected: %w", err))
		log.Printf("container inventory disconnected: %v", err)
		if !waitForInventoryRetry(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, i.options.maxBackoff)
	}
}

func (i *containerInventory) runConnected(ctx context.Context, runtimeClient containerRuntime) error {
	subscriptionContext, cancelSubscription := context.WithCancel(ctx)
	defer cancelSubscription()
	events, eventErrors := runtimeClient.Subscribe(subscriptionContext)

	containers, err := i.readSnapshot(ctx, runtimeClient)
	if err != nil {
		return err
	}
	connectedAt := time.Now().UTC()
	i.publishContainers(runtimeClient.Namespace(), runtimeClient.Backend(), connectedAt, containers)

	reconcile := time.NewTicker(i.options.reconcileInterval)
	defer reconcile.Stop()
	var debounce *time.Timer
	var debounceChannel <-chan time.Time
	scheduleRefresh := func(delay time.Duration) {
		if debounce == nil {
			debounce = time.NewTimer(delay)
		} else {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
			debounce.Reset(delay)
		}
		debounceChannel = debounce.C
	}
	defer func() {
		if debounce != nil {
			debounce.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case event, ok := <-events:
			if !ok {
				return errors.New("containerd event stream closed")
			}
			if i.recordEvent(event) {
				scheduleRefresh(i.options.debounce)
			}
		case err, ok := <-eventErrors:
			if !ok || err == nil {
				return errors.New("containerd event error stream closed")
			}
			return fmt.Errorf("containerd event stream: %w", err)
		case <-i.refresh:
			scheduleRefresh(0)
		case <-reconcile.C:
			scheduleRefresh(0)
		case <-debounceChannel:
			debounceChannel = nil
			containers, err := i.readSnapshot(ctx, runtimeClient)
			if err != nil {
				return err
			}
			i.publishContainers(runtimeClient.Namespace(), runtimeClient.Backend(), connectedAt, containers)
		}
	}
}

func (i *containerInventory) readSnapshot(ctx context.Context, runtimeClient containerRuntime) ([]Container, error) {
	snapshotContext, cancel := context.WithTimeout(ctx, i.options.operationTimeout)
	defer cancel()
	containers, err := runtimeClient.Snapshot(snapshotContext)
	if err != nil {
		return nil, fmt.Errorf("reconcile container inventory: %w", err)
	}
	return containers, nil
}

func waitForInventoryRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (i *containerInventory) triggerRefresh() {
	select {
	case i.refresh <- struct{}{}:
	default:
	}
}

func (i *containerInventory) recordEvent(event ContainerLifecycleEvent) bool {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.recordEventLocked(event)
}

func (i *containerInventory) recordEventLocked(event ContainerLifecycleEvent) bool {
	for index := len(i.snapshot.Events) - 1; index >= 0; index-- {
		existing := i.snapshot.Events[index]
		if existing.Topic == event.Topic &&
			existing.ContainerID == event.ContainerID &&
			existing.ExecID == event.ExecID &&
			existing.Timestamp.Equal(event.Timestamp) {
			return false
		}
	}
	i.sequence++
	event.Sequence = i.sequence
	i.snapshot.Events = append(i.snapshot.Events, event)
	if len(i.snapshot.Events) > maxInventoryEvents {
		i.snapshot.Events = append([]ContainerLifecycleEvent(nil), i.snapshot.Events[len(i.snapshot.Events)-maxInventoryEvents:]...)
	}
	i.snapshot.LastEventAt = event.Timestamp
	return true
}

func (i *containerInventory) publishContainers(
	namespace string,
	backend string,
	connectedAt time.Time,
	containers []Container,
) {
	sort.Slice(containers, func(left, right int) bool {
		leftName := strings.ToLower(firstNonEmpty(containers[left].Name, containers[left].ID))
		rightName := strings.ToLower(firstNonEmpty(containers[right].Name, containers[right].ID))
		if leftName == rightName {
			return containers[left].ID < containers[right].ID
		}
		return leftName < rightName
	})

	i.mu.Lock()
	previous := make(map[string]Container, len(i.snapshot.Containers))
	for _, container := range i.snapshot.Containers {
		previous[container.ID] = container
	}
	reconciledAt := time.Now().UTC()
	for _, container := range containers {
		if old, ok := previous[container.ID]; ok {
			i.recordReconciledTransitionsLocked(old, container, reconciledAt)
		}
	}
	for index := range containers {
		history := make([]ContainerLifecycleEvent, 0, maxContainerEvents)
		for eventIndex := len(i.snapshot.Events) - 1; eventIndex >= 0 && len(history) < maxContainerEvents; eventIndex-- {
			event := i.snapshot.Events[eventIndex]
			if event.ContainerID == containers[index].ID {
				history = append(history, event)
			}
		}
		for left, right := 0, len(history)-1; left < right; left, right = left+1, right-1 {
			history[left], history[right] = history[right], history[left]
		}
		containers[index].History = history
		if len(history) > 0 {
			last := history[len(history)-1]
			containers[index].LastTransition = last.Type
			containers[index].LastTransitionAt = last.Timestamp.Format(time.RFC3339Nano)
			var latestOOM time.Time
			var latestStart time.Time
			for eventIndex := len(history) - 1; eventIndex >= 0; eventIndex-- {
				event := history[eventIndex]
				if latestOOM.IsZero() && event.OOM {
					latestOOM = event.Timestamp
				}
				if latestStart.IsZero() && event.Type == "task-start" {
					latestStart = event.Timestamp
				}
			}
			if !latestOOM.IsZero() && (latestStart.IsZero() || latestOOM.After(latestStart)) {
				containers[index].OOMKilled = true
				containers[index].ExitReason = "oom"
			}
			if containers[index].ExitCode == nil {
				for eventIndex := len(history) - 1; eventIndex >= 0; eventIndex-- {
					event := history[eventIndex]
					if event.ExitCode != nil {
						code := *event.ExitCode
						containers[index].ExitCode = &code
						if event.ExitSignal != nil {
							signal := *event.ExitSignal
							containers[index].ExitSignal = &signal
						}
						if containers[index].ExitAt == "" {
							containers[index].ExitAt = event.Timestamp.Format(time.RFC3339Nano)
						}
						if containers[index].ExitReason == "" {
							containers[index].ExitReason = event.Reason
						}
						break
					}
				}
			}
		}
	}
	i.snapshot.Revision++
	i.snapshot.Available = true
	i.snapshot.Stale = false
	i.snapshot.Namespace = namespace
	i.snapshot.Backend = backend
	i.snapshot.Message = ""
	i.snapshot.ConnectedAt = connectedAt
	i.snapshot.LastReconciledAt = reconciledAt
	i.snapshot.Containers = containers
	i.snapshot.Capabilities = containerCapabilities()
	snapshot := cloneContainerSnapshot(i.snapshot)
	i.publishLocked(snapshot)
	i.mu.Unlock()
}

func (i *containerInventory) recordReconciledTransitionsLocked(old, current Container, timestamp time.Time) {
	appendEvent := func(eventType, reason string) {
		i.recordEventLocked(ContainerLifecycleEvent{
			Topic:       "/porto/reconcile/" + eventType,
			Type:        eventType,
			ContainerID: current.ID,
			Timestamp:   timestamp,
			Reason:      reason,
		})
	}
	if old.State != current.State && !i.hasStateTransitionEventLocked(old, current.State) {
		appendEvent("state-transition", old.State+"->"+current.State)
	}
	if old.Health.Status != current.Health.Status {
		appendEvent("health-transition", old.Health.Status+"->"+current.Health.Status)
	}
	if current.RestartCount > old.RestartCount {
		appendEvent("restart", fmt.Sprintf("count %d->%d", old.RestartCount, current.RestartCount))
	}
	if old.Networks != current.Networks || old.Ports != current.Ports ||
		!reflect.DeepEqual(old.NetworkDetails, current.NetworkDetails) {
		appendEvent("network-update", "network state changed")
	}
	if old.Resources != current.Resources {
		appendEvent("resource-update", "runtime resources changed")
	}
	if old.Name != current.Name || old.Image != current.Image ||
		!reflect.DeepEqual(old.Labels, current.Labels) ||
		!reflect.DeepEqual(old.Annotations, current.Annotations) {
		appendEvent("metadata-update", "container metadata changed")
	}
}

func (i *containerInventory) hasStateTransitionEventLocked(old Container, state string) bool {
	lastSequence := uint64(0)
	if len(old.History) > 0 {
		lastSequence = old.History[len(old.History)-1].Sequence
	}
	expected := map[string]map[string]bool{
		"running":    {"task-start": true, "task-resumed": true},
		"restarting": {"task-start": true},
		"paused":     {"task-paused": true},
		"pausing":    {"task-paused": true},
		"exited":     {"task-exit": true, "task-delete": true},
		"created":    {"task-create": true},
	}[state]
	for index := len(i.snapshot.Events) - 1; index >= 0; index-- {
		event := i.snapshot.Events[index]
		if event.Sequence <= lastSequence {
			break
		}
		if event.ContainerID == old.ID && expected[event.Type] {
			return true
		}
	}
	return false
}

func (i *containerInventory) markUnavailable(err error) {
	message := "container inventory unavailable"
	if err != nil {
		message = err.Error()
	}
	i.mu.Lock()
	if !i.snapshot.Available && i.snapshot.Message == message {
		i.mu.Unlock()
		return
	}
	i.snapshot.Revision++
	i.snapshot.Available = false
	i.snapshot.Stale = len(i.snapshot.Containers) > 0
	i.snapshot.Message = message
	i.snapshot.Capabilities = containerCapabilities()
	snapshot := cloneContainerSnapshot(i.snapshot)
	i.publishLocked(snapshot)
	i.mu.Unlock()
}

func (i *containerInventory) snapshotValue() ContainerSnapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return cloneContainerSnapshot(i.snapshot)
}

func (i *containerInventory) subscribe() (<-chan ContainerSnapshot, func()) {
	i.mu.Lock()
	i.nextID++
	id := i.nextID
	channel := make(chan ContainerSnapshot, 1)
	i.subscribers[id] = channel
	channel <- cloneContainerSnapshot(i.snapshot)
	i.mu.Unlock()
	return channel, func() {
		i.mu.Lock()
		if subscribed, ok := i.subscribers[id]; ok {
			delete(i.subscribers, id)
			close(subscribed)
		}
		i.mu.Unlock()
	}
}

func (i *containerInventory) closeSubscribers() {
	i.mu.Lock()
	for id, subscriber := range i.subscribers {
		delete(i.subscribers, id)
		close(subscriber)
	}
	i.mu.Unlock()
}

func (i *containerInventory) publishLocked(snapshot ContainerSnapshot) {
	for _, subscriber := range i.subscribers {
		select {
		case subscriber <- snapshot:
		default:
			select {
			case <-subscriber:
			default:
			}
			select {
			case subscriber <- snapshot:
			default:
			}
		}
	}
}

func cloneContainerSnapshot(snapshot ContainerSnapshot) ContainerSnapshot {
	cloned := snapshot
	cloned.Containers = make([]Container, len(snapshot.Containers))
	for index, container := range snapshot.Containers {
		cloned.Containers[index] = cloneContainer(container)
	}
	cloned.Events = append([]ContainerLifecycleEvent(nil), snapshot.Events...)
	for index := range cloned.Events {
		cloned.Events[index] = cloneContainerEvent(cloned.Events[index])
	}
	return cloned
}

func cloneContainer(container Container) Container {
	cloned := container
	cloned.Labels = cloneStringMap(container.Labels)
	cloned.Annotations = cloneStringMap(container.Annotations)
	cloned.NetworkDetails = append([]ContainerNetworkState(nil), container.NetworkDetails...)
	cloned.MountDetails = make([]ContainerMount, len(container.MountDetails))
	for index, mount := range container.MountDetails {
		cloned.MountDetails[index] = mount
		cloned.MountDetails[index].Options = append([]string(nil), mount.Options...)
	}
	cloned.History = append([]ContainerLifecycleEvent(nil), container.History...)
	for index := range cloned.History {
		cloned.History[index] = cloneContainerEvent(cloned.History[index])
	}
	if container.ExitCode != nil {
		value := *container.ExitCode
		cloned.ExitCode = &value
	}
	if container.ExitSignal != nil {
		value := *container.ExitSignal
		cloned.ExitSignal = &value
	}
	return cloned
}

func cloneContainerEvent(event ContainerLifecycleEvent) ContainerLifecycleEvent {
	cloned := event
	if event.ExitCode != nil {
		value := *event.ExitCode
		cloned.ExitCode = &value
	}
	if event.ExitSignal != nil {
		value := *event.ExitSignal
		cloned.ExitSignal = &value
	}
	return cloned
}

func (i *containerInventory) wait(ctx context.Context, id, condition string) (int, error) {
	updates, unsubscribe := i.subscribe()
	defer unsubscribe()
	initial := i.snapshotValue()
	container, err := findSnapshotContainer(initial, id)
	if err != nil {
		return 0, err
	}
	if condition != "next-exit" && !containerActive(container) {
		return containerExitCode(container), nil
	}
	knownExits := make(map[uint64]struct{})
	for _, event := range initial.Events {
		if event.ContainerID == container.ID && containerExitEvent(event) {
			knownExits[event.Sequence] = struct{}{}
		}
	}
	pendingExit := false

	for {
		select {
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		case snapshot, ok := <-updates:
			if !ok {
				return 0, errors.New("container inventory subscription closed")
			}
			if condition == "next-exit" {
				for _, event := range snapshot.Events {
					if event.ContainerID != container.ID || !containerExitEvent(event) {
						continue
					}
					if _, exists := knownExits[event.Sequence]; exists {
						continue
					}
					knownExits[event.Sequence] = struct{}{}
					pendingExit = true
					if event.ExitCode == nil {
						continue
					}
					return int(*event.ExitCode), nil
				}
				if pendingExit {
					current, findErr := findSnapshotContainer(snapshot, container.ID)
					if findErr == nil && !containerActive(current) && current.ExitCode != nil {
						return int(*current.ExitCode), nil
					}
					if findErr != nil && !strings.Contains(strings.ToLower(findErr.Error()), "not found") {
						return 0, findErr
					}
				}
				continue
			}
			current, findErr := findSnapshotContainer(snapshot, container.ID)
			if findErr != nil {
				if strings.Contains(strings.ToLower(findErr.Error()), "not found") {
					return 0, nil
				}
				return 0, findErr
			}
			if !containerActive(current) {
				return containerExitCode(current), nil
			}
		}
	}
}

func containerExitEvent(event ContainerLifecycleEvent) bool {
	return event.Type == "task-exit" || event.Type == "task-delete"
}

func findSnapshotContainer(snapshot ContainerSnapshot, id string) (*Container, error) {
	var matches []*Container
	normalizedID := strings.TrimPrefix(id, "/")
	for index := range snapshot.Containers {
		container := &snapshot.Containers[index]
		name := strings.TrimPrefix(container.Name, "/")
		if container.ID == normalizedID || name == normalizedID {
			return container, nil
		}
		if strings.HasPrefix(container.ID, normalizedID) {
			matches = append(matches, container)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("container identifier %q matched %d containers", id, len(matches))
	}
	return nil, fmt.Errorf("container %q not found", id)
}

func containerActive(container *Container) bool {
	switch container.State {
	case "running", "paused", "pausing", "restarting":
		return true
	default:
		return false
	}
}

func containerExitCode(container *Container) int {
	if container.ExitCode == nil {
		return 0
	}
	return int(*container.ExitCode)
}
