package resourcesaver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"os"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/config"
)

const (
	Disabled   = "disabled"
	EngineIdle = "engine-idle-suspend"
	Pause      = "container-pause"
	ScaleZero  = "container-scale-to-zero"
)

type Override struct {
	Mode        string `json:"mode,omitempty"`
	IdleSeconds int    `json:"idleSeconds,omitempty"`
}

type Config struct {
	Mode          string              `json:"mode"`
	IdleSeconds   int                 `json:"idleSeconds"`
	WakeSeconds   int                 `json:"wakeSeconds"`
	MaxPending    int                 `json:"maxPending"`
	MaxBodyBytes  int64               `json:"maxBodyBytes"`
	SuspendEngine bool                `json:"suspendEngine"`
	Exclusions    []string            `json:"exclusions"`
	Overrides     map[string]Override `json:"overrides"`
}

type Member struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Identity string `json:"identity"`
	PID      uint32 `json:"pid,omitempty"`
}

type Route struct {
	Hostname    string `json:"hostname"`
	ContainerID string `json:"containerId"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
}

type Application struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Members     []Member `json:"members"`
	Routes      []Route  `json:"routes"`
	Eligible    bool     `json:"eligible"`
	Busy        bool     `json:"busy"`
	Reason      string   `json:"reason,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	IdleSeconds int      `json:"idleSeconds,omitempty"`
}

type Snapshot struct {
	Complete      bool
	EngineRunning bool
	Applications  []Application
	Message       string
}

type Entry struct {
	Application Application `json:"application"`
	Mode        string      `json:"mode"`
	Token       string      `json:"token"`
	Phase       string      `json:"phase"`
	UpdatedAt   time.Time   `json:"updatedAt"`
	Error       string      `json:"error,omitempty"`
}

type Event struct {
	At          time.Time `json:"at"`
	Application string    `json:"application"`
	Action      string    `json:"action"`
	Message     string    `json:"message,omitempty"`
}

type State struct {
	Config      Config           `json:"config"`
	Entries     map[string]Entry `json:"entries"`
	EngineToken string           `json:"engineToken,omitempty"`
	Events      []Event          `json:"events"`
	Message     string           `json:"message,omitempty"`
	Pending     int              `json:"pending"`
}

type Backend interface {
	Snapshot(context.Context) (Snapshot, error)
	Sleep(context.Context, Entry) error
	Resume(context.Context, Entry) error
	Ready(context.Context, Application) (bool, error)
	SuspendEngine(context.Context, string) error
	WakeEngine(context.Context, string) error
}

type OwnershipUncertainError struct{ Err error }

func (e OwnershipUncertainError) Error() string { return e.Err.Error() }
func (e OwnershipUncertainError) Unwrap() error { return e.Err }

type operation struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waking  bool
	err     error
	waiters int
}

type Manager struct {
	mu             sync.Mutex
	state          State
	path           string
	backend        Backend
	ctx            context.Context
	cancel         context.CancelFunc
	operations     map[string]*operation
	active         map[string]int
	lastActivity   map[string]time.Time
	engineIdleAt   time.Time
	workers        sync.WaitGroup
	now            func() time.Time
	persistenceErr error
}

func Defaults() Config {
	return Config{Mode: Disabled, IdleSeconds: 300, WakeSeconds: 60, MaxPending: 64,
		MaxBodyBytes: 16 << 20, Exclusions: []string{}, Overrides: map[string]Override{}}
}

func Validate(configuration Config) error {
	switch configuration.Mode {
	case Disabled, EngineIdle, Pause, ScaleZero:
	default:
		return errors.New("Resource Saver mode must be disabled, engine-idle-suspend, container-pause, or container-scale-to-zero")
	}
	if configuration.IdleSeconds < 1 || configuration.IdleSeconds > 86400 ||
		configuration.WakeSeconds < 1 || configuration.WakeSeconds > 1200 ||
		configuration.MaxPending < 1 || configuration.MaxPending > 1024 ||
		configuration.MaxBodyBytes < 1 || configuration.MaxBodyBytes > 2<<30 {
		return errors.New("Resource Saver requires idle=1..86400s, wake=1..1200s, pending=1..1024, and queued body limit=1 byte..2 GiB")
	}
	for _, pattern := range configuration.Exclusions {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid Resource Saver exclusion %q: %w", pattern, err)
		}
	}
	for name, override := range configuration.Overrides {
		if name == "" || override.IdleSeconds < 0 || override.IdleSeconds > 86400 ||
			(override.Mode != "" && override.Mode != Disabled && override.Mode != Pause && override.Mode != ScaleZero) {
			return fmt.Errorf("invalid Resource Saver override for %q", name)
		}
	}
	return nil
}

func New(parent context.Context, statePath string, backend Backend) (*Manager, error) {
	if backend == nil {
		return nil, errors.New("Resource Saver backend is required")
	}
	state := State{Config: Defaults(), Entries: map[string]Entry{}, Events: []Event{}}
	data, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("decode Resource Saver ownership journal: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := Validate(state.Config); err != nil {
		return nil, err
	}
	if state.Entries == nil {
		state.Entries = map[string]Entry{}
	}
	for id, entry := range state.Entries {
		if entry.Application.ID != id || entry.Token == "" || len(entry.Application.Members) == 0 ||
			entry.Mode != Pause && entry.Mode != ScaleZero {
			return nil, errors.New("Resource Saver journal has invalid application ownership; no workload was adopted")
		}
		for _, member := range entry.Application.Members {
			if member.ID == "" || member.Identity == "" {
				return nil, errors.New("Resource Saver journal has an incomplete workload identity")
			}
		}
		for _, route := range entry.Application.Routes {
			ip := net.ParseIP(route.Host)
			if ip == nil || !ip.IsLoopback() || route.Port < 1 || route.Port > 65535 {
				return nil, errors.New("Resource Saver journal contains an invalid or non-loopback wake route")
			}
		}
		if entry.Mode == Pause {
			entry.Phase = "recovery-required"
			entry.Error = "Paused ownership cannot be proven across daemon downtime; resume this container manually before opting in again"
			state.Entries[id] = entry
		}
	}
	state.Pending = 0
	ctx, cancel := context.WithCancel(parent)
	return &Manager{state: state, path: statePath, backend: backend, ctx: ctx, cancel: cancel,
		operations: map[string]*operation{}, active: map[string]int{}, lastActivity: map[string]time.Time{}, now: time.Now}, nil
}

func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	state.Config.Exclusions = slices.Clone(state.Config.Exclusions)
	state.Config.Overrides = maps.Clone(state.Config.Overrides)
	state.Events = slices.Clone(state.Events)
	state.Entries = maps.Clone(state.Entries)
	for id, entry := range state.Entries {
		entry.Application.Members = slices.Clone(entry.Application.Members)
		entry.Application.Routes = slices.Clone(entry.Application.Routes)
		state.Entries[id] = entry
	}
	return state
}

func (m *Manager) Configure(configuration Config) error {
	if err := Validate(configuration); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return context.Cause(m.ctx)
	}
	previous := m.state.Config
	m.state.Config = configuration
	if err := m.saveLocked(); err != nil {
		m.state.Config = previous
		return err
	}
	for _, operation := range m.operations {
		operation.cancel()
	}
	m.lastActivity = map[string]time.Time{}
	m.engineIdleAt = time.Time{}
	return nil
}

func (m *Manager) Run(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := m.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Resource Saver: %v", err)
			m.mu.Lock()
			m.state.Message = err.Error()
			m.eventLocked("", "failure", err.Error())
			m.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-m.ctx.Done():
			return context.Cause(m.ctx)
		case <-ticker.C:
		}
	}
}

func (m *Manager) Tick(ctx context.Context) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return context.Cause(m.ctx)
	}
	if m.persistenceErr != nil {
		if err := m.saveLocked(); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	configuration := m.state.Config
	m.mu.Unlock()
	if configuration.Mode == Disabled {
		return nil
	}
	snapshot, err := m.backend.Snapshot(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return context.Cause(m.ctx)
	}
	now := m.now()
	if !snapshot.Complete {
		m.state.Message = "Resource Saver blocked by incomplete inventory: " + snapshot.Message
		m.engineIdleAt = time.Time{}
		return nil
	}
	m.state.Message = ""
	idleEngine := snapshot.EngineRunning
	for _, application := range snapshot.Applications {
		_, owned := m.state.Entries[application.ID]
		if !owned {
			idleEngine = false
		}
		mode, timeout := m.policyLocked(application)
		if mode == Disabled || mode == EngineIdle || !application.Eligible || application.Busy || m.active[application.ID] > 0 || m.operations[application.ID] != nil {
			m.lastActivity[application.ID] = now
			continue
		}
		last, exists := m.lastActivity[application.ID]
		if !exists {
			m.lastActivity[application.ID] = now
			continue
		}
		if owned || now.Sub(last) < time.Duration(timeout)*time.Second {
			continue
		}
		token, err := newToken()
		if err != nil {
			return err
		}
		entry := Entry{Application: application, Mode: mode, Token: token, Phase: "suspending", UpdatedAt: now}
		m.state.Entries[application.ID] = entry
		if err := m.saveLocked(); err != nil {
			delete(m.state.Entries, application.ID)
			return err
		}
		operationContext, cancel := context.WithTimeout(m.ctx, time.Duration(configuration.WakeSeconds)*time.Second)
		op := &operation{done: make(chan struct{}), cancel: cancel}
		m.operations[application.ID] = op
		m.workers.Add(1)
		go m.sleep(operationContext, entry, op)
	}
	if !idleEngine || len(m.operations) > 0 || m.state.Pending > 0 || !configuration.SuspendEngine && configuration.Mode != EngineIdle {
		m.engineIdleAt = time.Time{}
		return nil
	}
	if m.state.EngineToken != "" {
		return nil
	}
	if m.engineIdleAt.IsZero() {
		m.engineIdleAt = now
		return nil
	}
	if now.Sub(m.engineIdleAt) < time.Duration(configuration.IdleSeconds)*time.Second {
		return nil
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	m.state.EngineToken = token
	if err := m.saveLocked(); err != nil {
		m.state.EngineToken = ""
		return err
	}
	operationContext, cancel := context.WithTimeout(m.ctx, time.Duration(configuration.WakeSeconds)*time.Second)
	op := &operation{done: make(chan struct{}), cancel: cancel}
	m.operations["engine"] = op
	m.workers.Add(1)
	go m.suspendEngine(operationContext, token, op)
	return nil
}

func (m *Manager) policyLocked(application Application) (string, int) {
	mode, timeout := m.state.Config.Mode, m.state.Config.IdleSeconds
	if application.Mode != "" {
		mode = application.Mode
	}
	if application.IdleSeconds > 0 {
		timeout = application.IdleSeconds
	}
	for _, exclusion := range m.state.Config.Exclusions {
		for _, member := range application.Members {
			if matched, _ := path.Match(exclusion, member.Name); matched {
				return Disabled, timeout
			}
		}
		if matched, _ := path.Match(exclusion, application.Name); matched {
			return Disabled, timeout
		}
	}
	if override, exists := m.state.Config.Overrides[application.ID]; exists {
		if override.Mode != "" {
			mode = override.Mode
		}
		if override.IdleSeconds > 0 {
			timeout = override.IdleSeconds
		}
	}
	return mode, timeout
}

func (m *Manager) sleep(ctx context.Context, entry Entry, op *operation) {
	defer m.workers.Done()
	err := m.backend.Sleep(ctx, entry)
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.UpdatedAt = m.now()
	if err == nil {
		entry.Phase = "sleeping"
		m.eventLocked(entry.Application.Name, entry.Mode, "")
	} else {
		entry.Phase, entry.Error = "failed", err.Error()
		m.eventLocked(entry.Application.Name, "suspend-failed", err.Error())
	}
	var uncertain OwnershipUncertainError
	if err != nil && !errors.As(err, &uncertain) {
		delete(m.state.Entries, entry.Application.ID)
		m.lastActivity[entry.Application.ID] = m.now()
	} else {
		m.state.Entries[entry.Application.ID] = entry
	}
	err = errors.Join(err, m.saveLocked())
	m.finishLocked(entry.Application.ID, op, err)
}

func (m *Manager) suspendEngine(ctx context.Context, token string, op *operation) {
	defer m.workers.Done()
	err := m.backend.SuspendEngine(ctx, token)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.state.EngineToken = ""
		m.engineIdleAt = m.now()
		m.eventLocked("", "engine-suspend-failed", err.Error())
	} else {
		m.eventLocked("", "engine-suspended", "")
	}
	err = errors.Join(err, m.saveLocked())
	m.finishLocked("engine", op, err)
}

func (m *Manager) Acquire(ctx context.Context, applicationID string) (func(), error) {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, context.Cause(m.ctx)
	}
	if m.state.Pending >= m.state.Config.MaxPending {
		m.mu.Unlock()
		return nil, errors.New("Resource Saver wake queue is full; retry after current requests finish waking")
	}
	m.state.Pending++
	m.active[applicationID]++
	m.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.mu.Lock()
			m.state.Pending--
			m.active[applicationID]--
			m.lastActivity[applicationID] = m.now()
			if m.state.Pending == 0 {
				if operation := m.operations["engine"]; operation != nil && operation.waking {
					operation.cancel()
				}
			}
			m.mu.Unlock()
		})
	}
	for {
		m.mu.Lock()
		if op := m.operations[applicationID]; op != nil {
			op.waiters++
			m.mu.Unlock()
			select {
			case <-op.done:
				if op.err != nil && op.waking {
					release()
					return nil, op.err
				}
			case <-ctx.Done():
				m.mu.Lock()
				op.waiters--
				if op.waking && op.waiters == 0 {
					op.cancel()
				}
				m.mu.Unlock()
				release()
				return nil, context.Cause(ctx)
			}
			continue
		}
		entry, owned := m.state.Entries[applicationID]
		if !owned {
			m.mu.Unlock()
			return release, nil
		}
		if entry.Phase == "recovery-required" {
			m.mu.Unlock()
			release()
			return nil, errors.New(entry.Error)
		}
		wakeContext, cancel := context.WithTimeout(m.ctx, time.Duration(m.state.Config.WakeSeconds)*time.Second)
		op := &operation{done: make(chan struct{}), cancel: cancel, waking: true}
		m.operations[applicationID] = op
		entry.Phase, entry.UpdatedAt, entry.Error = "waking", m.now(), ""
		m.state.Entries[applicationID] = entry
		if err := m.saveLocked(); err != nil {
			delete(m.operations, applicationID)
			cancel()
			m.mu.Unlock()
			release()
			return nil, err
		}
		m.workers.Add(1)
		go m.wake(wakeContext, entry, op)
		m.mu.Unlock()
	}
}

func (m *Manager) wake(ctx context.Context, entry Entry, op *operation) {
	defer m.workers.Done()
	err := m.wakeEngine(ctx)
	if err == nil {
		err = m.backend.Resume(ctx, entry)
	}
	if err == nil {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			ready, readinessErr := m.backend.Ready(ctx, entry.Application)
			if readinessErr != nil {
				err = readinessErr
				break
			}
			if ready {
				break
			}
			select {
			case <-ctx.Done():
				err = fmt.Errorf("Resource Saver readiness deadline: %w", context.Cause(ctx))
			case <-ticker.C:
				continue
			}
			break
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		entry.Phase, entry.Error, entry.UpdatedAt = "failed", err.Error(), m.now()
		m.state.Entries[entry.Application.ID] = entry
		m.eventLocked(entry.Application.Name, "wake-failed", err.Error())
	} else {
		delete(m.state.Entries, entry.Application.ID)
		m.eventLocked(entry.Application.Name, "resumed", "")
	}
	err = errors.Join(err, m.saveLocked())
	m.finishLocked(entry.Application.ID, op, err)
}

func (m *Manager) wakeEngine(ctx context.Context) error {
	for {
		m.mu.Lock()
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return context.Cause(ctx)
		}
		if err := m.ctx.Err(); err != nil {
			m.mu.Unlock()
			return context.Cause(m.ctx)
		}
		token := m.state.EngineToken
		if token == "" {
			m.mu.Unlock()
			return nil
		}
		if op := m.operations["engine"]; op != nil {
			m.mu.Unlock()
			select {
			case <-op.done:
				if op.err != nil {
					return op.err
				}
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			continue
		}
		wakeContext, cancel := context.WithTimeout(m.ctx, time.Duration(m.state.Config.WakeSeconds)*time.Second)
		op := &operation{done: make(chan struct{}), cancel: cancel, waking: true}
		m.operations["engine"] = op
		m.workers.Add(1)
		go func() {
			defer m.workers.Done()
			err := m.backend.WakeEngine(wakeContext, token)
			m.mu.Lock()
			defer m.mu.Unlock()
			if err == nil {
				m.state.EngineToken = ""
				m.eventLocked("", "engine-resumed", "")
			} else {
				m.eventLocked("", "engine-wake-failed", err.Error())
			}
			err = errors.Join(err, m.saveLocked())
			m.finishLocked("engine", op, err)
		}()
		m.mu.Unlock()
	}
}

func (m *Manager) Forget(applicationID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operations[applicationID] != nil {
		return errors.New("finish or cancel the Resource Saver transition before changing this workload manually")
	}
	delete(m.state.Entries, applicationID)
	m.lastActivity[applicationID] = m.now()
	return m.saveLocked()
}

func (m *Manager) WakeEngine(ctx context.Context) error { return m.wakeEngine(ctx) }

func (m *Manager) ForgetEngine() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operations["engine"] != nil {
		return errors.New("finish the Resource Saver engine transition before changing the engine manually")
	}
	m.state.EngineToken = ""
	return m.saveLocked()
}

func (m *Manager) ForgetMember(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, entry := range m.state.Entries {
		found := false
		for _, member := range entry.Application.Members {
			if member.ID == id || member.Name == id {
				found = true
			}
		}
		if !found {
			continue
		}
		if m.operations[key] != nil {
			return errors.New("finish or cancel the Resource Saver transition before changing this workload manually")
		}
		members := make([]Member, 0, len(entry.Application.Members))
		for _, member := range entry.Application.Members {
			if member.ID != id && member.Name != id {
				members = append(members, member)
			}
		}
		routes := make([]Route, 0, len(entry.Application.Routes))
		for _, route := range entry.Application.Routes {
			if route.ContainerID != id {
				routes = append(routes, route)
			}
		}
		entry.Application.Members, entry.Application.Routes = members, routes
		if len(members) == 0 {
			delete(m.state.Entries, key)
		} else {
			m.state.Entries[key] = entry
		}
		m.lastActivity[key] = m.now()
		m.eventLocked(entry.Application.Name, "manual-ownership", "Only the explicitly selected member was released from Resource Saver ownership")
		return m.saveLocked()
	}
	return nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.cancel()
	done := make(chan struct{})
	go func() { m.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (m *Manager) finishLocked(id string, op *operation, err error) {
	op.err = err
	op.cancel()
	delete(m.operations, id)
	close(op.done)
}

func (m *Manager) eventLocked(application, action, message string) {
	m.state.Events = append(m.state.Events, Event{At: m.now(), Application: application, Action: action, Message: message})
	if len(m.state.Events) > 200 {
		m.state.Events = m.state.Events[len(m.state.Events)-200:]
	}
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(m.path, append(data, '\n'), 0o600); err != nil {
		m.state.Message = "Resource Saver ownership could not be persisted: " + err.Error()
		m.persistenceErr = err
		return err
	}
	m.persistenceErr = nil
	return nil
}

func newToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}
