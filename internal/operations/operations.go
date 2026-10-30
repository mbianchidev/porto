package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

var ErrBusy = errors.New("a local operation already owns this resource or the queue is full")

type Operation struct {
	ID          string `json:"id"`
	Resource    string `json:"resource"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	Phase       string `json:"phase"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt,omitempty"`
}

type failure struct{ message, status string }

func (e failure) Error() string    { return e.message }
func Failure(message string) error { return failure{message, "failed"} }
func Partial(message string) error { return failure{message, "partial"} }

func PublicMessage(err error) string {
	var safe failure
	if errors.As(err, &safe) {
		return safe.message
	}
	if errors.Is(err, context.Canceled) {
		return "Operation canceled; inspect current resource state before retrying."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Operation timed out; inspect current resource conditions and retry explicitly."
	}
	return "Local operation failed; check tool availability, permissions and local storage, then retry."
}

type job struct {
	operation Operation
	cancel    context.CancelFunc
	done      chan struct{}
}

type Manager struct {
	root         string
	mu           sync.Mutex
	jobs         map[string]*job
	resources    map[string]string
	reservations map[string]*manualReservation
	slots        chan struct{}
	closed       bool
}

func New(root string) *Manager {
	return &Manager{root: root, jobs: map[string]*job{}, resources: map[string]string{}, reservations: map[string]*manualReservation{}, slots: make(chan struct{}, 2)}
}

func (m *Manager) Start(ctx context.Context, resource, action string, work func(context.Context, func(string) error) error) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || len(m.resources) >= 16 || m.resources[resource] != "" {
		return Operation{}, ErrBusy
	}
	if ctx.Err() != nil {
		return Operation{}, ctx.Err()
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return Operation{}, err
	}
	value := Operation{ID: hex.EncodeToString(id), Resource: resource, Action: action, Status: "queued", Phase: "waiting", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := m.save(value); err != nil {
		return value, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	current := &job{operation: value, cancel: cancel, done: make(chan struct{})}
	m.jobs[value.ID], m.resources[resource] = current, value.ID
	go m.run(ctx, current, work)
	return value, nil
}

func (m *Manager) run(ctx context.Context, current *job, work func(context.Context, func(string) error) error) {
	defer current.cancel()
	var err error
	select {
	case m.slots <- struct{}{}:
		progress := func(phase string) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			current.operation.Status, current.operation.Phase = "running", phase
			return m.save(current.operation)
		}
		err = progress("starting")
		if err == nil {
			err = work(m.ownerContext(ctx, current.operation.Resource, current.operation.ID), progress)
		}
		<-m.slots
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current.operation.Status, current.operation.Phase = "succeeded", "complete"
	if err != nil {
		current.operation.Status, current.operation.Error = "failed", PublicMessage(err)
		var safe failure
		if errors.As(err, &safe) {
			current.operation.Status = safe.status
		} else if errors.Is(err, context.Canceled) {
			current.operation.Status = "canceled"
		}
		slog.Warn("Local operation did not complete", "id", current.operation.ID, "action", current.operation.Action, "status", current.operation.Status)
	}
	current.operation.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := m.save(current.operation); err != nil {
		slog.Error("Unable to persist local operation", "id", current.operation.ID, "error", err)
		current.operation.Status, current.operation.Error = "failed", "Unable to persist operation history; check local storage."
	}
	delete(m.resources, current.operation.Resource)
	delete(m.jobs, current.operation.ID)
	close(current.done)
}

func (m *Manager) save(value Operation) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, value.ID+".json"), data, 0o600)
}

func ValidID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (m *Manager) Get(id string) (Operation, error) {
	if !ValidID(id) {
		return Operation{}, errors.New("invalid operation ID")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.jobs[id]; current != nil {
		return current.operation, nil
	}
	var value Operation
	data, err := os.ReadFile(filepath.Join(m.root, id+".json"))
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	if value.Status == "queued" || value.Status == "running" {
		value.Status, value.Error = "failed", "Daemon restarted during the operation. Inspect current state before retrying; no success or rollback is implied."
	}
	return value, nil
}

func (m *Manager) List() ([]Operation, error) {
	entries, err := os.ReadDir(m.root)
	if errors.Is(err, os.ErrNotExist) {
		return []Operation{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := []Operation{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		value, err := m.Get(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b Operation) int { return strings.Compare(b.StartedAt, a.StartedAt) })
	return values, nil
}

func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.jobs[id]; current != nil {
		current.cancel()
		return nil
	}
	return errors.New("operation is not running")
}

func (m *Manager) Busy(resource string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resources[resource] != ""
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	jobs := make([]*job, 0, len(m.jobs))
	for _, current := range m.jobs {
		current.cancel()
		jobs = append(jobs, current)
	}
	manual := make([]*manualReservation, 0, len(m.reservations))
	for _, reservation := range m.reservations {
		reservation.cancel()
		manual = append(manual, reservation)
	}
	m.mu.Unlock()
	for _, current := range jobs {
		select {
		case <-current.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for local operation cleanup: %w", ctx.Err())
		}
	}
	for _, reservation := range manual {
		select {
		case <-reservation.done:
		case <-ctx.Done():
			return fmt.Errorf("wait for project action cleanup: %w", ctx.Err())
		}
	}
	return nil
}
