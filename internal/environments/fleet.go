package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/operations"
)

type RequestFunc func(context.Context, string, string, string, json.RawMessage) (json.RawMessage, error)
type FleetTarget struct {
	EnvironmentID   string `json:"environmentId"`
	ResourceID      string `json:"resourceId"`
	EnvironmentName string `json:"environmentName,omitempty"`
	Revision        uint64 `json:"revision"`
	State           string `json:"state,omitempty"`
}
type FleetRequest struct {
	Action  string        `json:"action"`
	Targets []FleetTarget `json:"targets"`
}
type FleetPreview struct {
	ID           string        `json:"id"`
	Action       string        `json:"action"`
	Targets      []FleetTarget `json:"targets"`
	Confirmation string        `json:"confirmation"`
	ExpiresAt    time.Time     `json:"expiresAt"`
}
type TargetResult struct {
	Target FleetTarget `json:"target"`
	Status string      `json:"status"`
	Error  string      `json:"error,omitempty"`
}
type FleetRun struct {
	ID        string         `json:"id"`
	Operation string         `json:"operation"`
	Action    string         `json:"action"`
	Results   []TargetResult `json:"results"`
}
type Fleet struct {
	manager    *Manager
	operations *operations.Manager
	request    RequestFunc
	root       string
	mu         sync.Mutex
	previews   map[string]FleetPreview
	active     map[string]string
}

func NewFleet(root string, manager *Manager, ops *operations.Manager, request RequestFunc) *Fleet {
	return &Fleet{root: root, manager: manager, operations: ops, request: request, previews: map[string]FleetPreview{}, active: map[string]string{}}
}

var containerIdentity = regexp.MustCompile(`^[a-fA-F0-9]{12,64}$`)

func (f *Fleet) resolve(ctx context.Context, target FleetTarget) (FleetTarget, error) {
	p, err := f.manager.Profile(target.EnvironmentID)
	if err != nil {
		return target, err
	}
	if p.Revoked || !slices.Contains(p.Capabilities, WorkloadOperate) {
		return target, problem(403, "fleet_authorization", "Each fleet target needs an active workload.operate grant.")
	}
	data, err := f.request(ctx, p.ID, "GET", "/api/docker/containers/"+url.PathEscape(target.ResourceID)+"/identity", nil)
	if err != nil {
		return target, err
	}
	var inspection struct {
		ID    string `json:"Id"`
		State struct{ Status string }
	}
	if err := json.Unmarshal(data, &inspection); err != nil || !containerIdentity.MatchString(inspection.ID) {
		return target, problem(502, "fleet_identity", "The target did not return an immutable container identity; no action is authorized by name alone.")
	}
	return FleetTarget{EnvironmentID: p.ID, ResourceID: inspection.ID, EnvironmentName: p.Name, Revision: p.Revision, State: inspection.State.Status}, nil
}
func (f *Fleet) Preview(ctx context.Context, request FleetRequest) (FleetPreview, error) {
	if !slices.Contains([]string{"start", "stop", "restart", "remove"}, request.Action) || len(request.Targets) == 0 || len(request.Targets) > 32 {
		return FleetPreview{}, problem(400, "fleet_request", "Choose start, stop, restart or remove for 1-32 explicitly selected containers.")
	}
	id, err := ID()
	if err != nil {
		return FleetPreview{}, err
	}
	preview := FleetPreview{ID: id, Action: request.Action, Targets: []FleetTarget{}, ExpiresAt: time.Now().Add(2 * time.Minute).UTC(), Confirmation: fmt.Sprintf("%s %d targets", request.Action, len(request.Targets))}
	seen := map[string]bool{}
	for _, target := range request.Targets {
		resolved, err := f.resolve(ctx, target)
		if err != nil {
			return FleetPreview{}, err
		}
		key := resolved.EnvironmentID + ":" + resolved.ResourceID
		if seen[key] {
			return FleetPreview{}, problem(400, "fleet_duplicate", "A container may appear only once per environment in a fleet preview.")
		}
		seen[key] = true
		preview.Targets = append(preview.Targets, resolved)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, value := range f.previews {
		if time.Now().After(value.ExpiresAt) {
			delete(f.previews, id)
		}
	}
	if len(f.previews) >= 128 {
		return FleetPreview{}, problem(429, "fleet_limit", "Too many active fleet previews; wait for expiry.")
	}
	f.previews[id] = preview
	return preview, nil
}
func (f *Fleet) save(run FleetRun) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(f.root, run.ID+".json"), data, 0o600)
}
func (f *Fleet) Start(ctx context.Context, id, confirmation string) (FleetRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	preview, ok := f.previews[id]
	if !ok || !time.Now().Before(preview.ExpiresAt) || confirmation != preview.Confirmation {
		return FleetRun{}, problem(409, "fleet_preview", "The exact unexpired preview and target-count confirmation are required.")
	}
	for _, target := range preview.Targets {
		if f.active[target.EnvironmentID+":"+target.ResourceID] != "" {
			return FleetRun{}, problem(409, "fleet_busy", "Another fleet action owns one of these exact targets.")
		}
	}
	runID, err := ID()
	if err != nil {
		return FleetRun{}, err
	}
	run := FleetRun{ID: runID, Action: preview.Action, Results: []TargetResult{}}
	for _, target := range preview.Targets {
		run.Results = append(run.Results, TargetResult{Target: target, Status: "pending"})
	}
	if err := f.save(run); err != nil {
		return FleetRun{}, err
	}
	for _, target := range preview.Targets {
		f.active[target.EnvironmentID+":"+target.ResourceID] = runID
	}
	admitted := make(chan struct{})
	work := func(ctx context.Context, progress func(string) error) error {
		<-admitted
		defer func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, target := range preview.Targets {
				key := target.EnvironmentID + ":" + target.ResourceID
				if f.active[key] == runID {
					delete(f.active, key)
				}
			}
		}()
		if err := progress("revalidating exact environment and container identities; no offline replay"); err != nil {
			return err
		}
		slots := make(chan struct{}, 4)
		var workers sync.WaitGroup
		var mu sync.Mutex
		var failures []error
		for i := range run.Results {
			workers.Add(1)
			go func(index int) {
				defer workers.Done()
				target := preview.Targets[index]
				var err error
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
				case <-ctx.Done():
					err = ctx.Err()
				}
				if err == nil && time.Now().After(preview.ExpiresAt) {
					err = problem(409, "fleet_expired", "Preview expired before this target could start. Create a fresh preview.")
				}
				if err == nil {
					current, resolveErr := f.resolve(ctx, target)
					err = resolveErr
					if err == nil && (current.ResourceID != target.ResourceID || current.Revision != target.Revision) {
						err = problem(409, "fleet_changed", "Environment identity or container changed. Preview the exact target again; the action was not retargeted.")
					}
				}
				if err == nil {
					_, err = f.request(ctx, target.EnvironmentID, "POST", "/api/docker/containers/"+url.PathEscape(target.ResourceID)+"/"+preview.Action, nil)
				}
				mu.Lock()
				defer mu.Unlock()
				run.Results[index].Status = "succeeded"
				if err != nil {
					run.Results[index].Status, run.Results[index].Error = "failed", err.Error()
					if ctx.Err() != nil {
						run.Results[index].Status = "canceled"
						run.Results[index].Error = "Canceled. An already dispatched side effect may be unconfirmed; inspect this exact target before retrying."
					}
					failures = append(failures, err)
				}
				if err := f.save(run); err != nil {
					failures = append(failures, err)
				}
			}(i)
		}
		workers.Wait()
		if len(failures) > 0 {
			return operations.Partial("One or more fleet targets failed, were canceled or became unavailable. Inspect per-target results; successful effects are not rolled back or replayed.")
		}
		return nil
	}
	operation, err := f.operations.Start(ctx, "fleet:"+runID, "fleet."+preview.Action, work)
	if err != nil {
		for _, target := range preview.Targets {
			delete(f.active, target.EnvironmentID+":"+target.ResourceID)
		}
		return FleetRun{}, err
	}
	run.Operation = operation.ID
	if err := f.save(run); err != nil {
		cancelErr := f.operations.Cancel(operation.ID)
		close(admitted)
		return FleetRun{}, errors.Join(err, cancelErr)
	}
	delete(f.previews, id)
	initial := run
	initial.Results = slices.Clone(run.Results)
	close(admitted)
	go func() {
		if _, err := f.operations.Wait(context.Background(), operation.ID); err != nil {
			slog.Error("Wait for fleet ownership cleanup", "operation", operation.ID, "error", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, target := range preview.Targets {
			key := target.EnvironmentID + ":" + target.ResourceID
			if f.active[key] == runID {
				delete(f.active, key)
			}
		}
	}()
	return initial, nil
}
func (f *Fleet) Get(id string) (FleetRun, error) {
	var run FleetRun
	if !safeID.MatchString(id) {
		return run, problem(400, "fleet_id", "Invalid fleet operation identity.")
	}
	data, err := os.ReadFile(filepath.Join(f.root, id+".json"))
	if err != nil {
		return run, err
	}
	err = json.Unmarshal(data, &run)
	if err == nil && run.Operation != "" {
		operation, operationErr := f.operations.Get(run.Operation)
		if operationErr != nil {
			return run, operationErr
		}
		if operation.Status != "running" && operation.Status != "queued" {
			for i := range run.Results {
				if run.Results[i].Status == "pending" {
					run.Results[i].Status, run.Results[i].Error = "interrupted", "Daemon stopped or canceled before a final result was recorded. Inspect this exact target before explicitly retrying; no action is replayed."
				}
			}
		}
	}
	return run, err
}

func (f *Fleet) List() ([]FleetRun, error) {
	entries, err := os.ReadDir(f.root)
	if errors.Is(err, os.ErrNotExist) {
		return []FleetRun{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := []FleetRun{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !safeID.MatchString(id) {
			continue
		}
		value, err := f.Get(id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, nil
}
