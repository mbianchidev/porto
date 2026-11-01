package extensions

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/blang/semver/v4"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/environments"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

type diskState struct {
	Version    int                  `json:"version"`
	Installed  map[string]Installed `json:"installed"`
	Publishers map[string]string    `json:"publishers"`
	Catalog    []CatalogEntry       `json:"catalog"`
}
type running struct {
	cancel    context.CancelFunc
	done      chan struct{}
	extension string
}
type Manager struct {
	root     string
	vault    credentials.Store
	runtime  wazero.Runtime
	compiled map[string]wazero.CompiledModule
	broker   RuntimeBroker
	mu       sync.Mutex
	state    diskState
	active   map[string]*running
	slots    chan struct{}
	closed   bool
}

func failure(status int, code, message string) error {
	return &environments.Error{Status: status, Code: code, Message: message}
}

func Open(root string, vault credentials.Store, broker RuntimeBroker) (*Manager, error) {
	if vault == nil {
		vault = credentials.System{}
	}
	m := &Manager{root: root, vault: credentials.NewChunked(vault), broker: broker, active: map[string]*running{}, compiled: map[string]wazero.CompiledModule{}, slots: make(chan struct{}, 2), state: diskState{Version: 1, Installed: map[string]Installed{}, Publishers: map[string]string{}, Catalog: []CatalogEntry{}}}
	data, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err == nil {
		if err := json.Unmarshal(data, &m.state); err != nil {
			return nil, fmt.Errorf("read installed extension state: %w", err)
		}
		if m.state.Version != 1 || m.state.Installed == nil || m.state.Publishers == nil {
			return nil, errors.New("unsupported or incomplete extension state; restore its previous state file")
		}
		for id, value := range m.state.Installed {
			if id != value.ID || !extensionID.MatchString(id) || !digestPattern.MatchString(value.SHA256) {
				return nil, errors.New("invalid installed extension identity")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	m.runtime = wazero.NewRuntimeWithConfig(context.Background(), wazero.NewRuntimeConfig().WithMemoryLimitPages(1024).WithCloseOnContextDone(true).WithDebugInfoEnabled(false))
	if _, err := wasi_snapshot_preview1.Instantiate(context.Background(), m.runtime); err != nil {
		return nil, errors.Join(err, m.runtime.Close(context.Background()))
	}
	return m, nil
}

func (m *Manager) saveLocked() error {
	data, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(filepath.Join(m.root, "state.json"), data, 0o600)
}

func validateManifest(value Manifest) error {
	if value.APIVersion != APIVersion {
		return failure(409, "extension_api", "Incompatible extension API. Porto supports only "+APIVersion+".")
	}
	if !extensionID.MatchString(value.ID) || len(value.ID) > 128 || strings.TrimSpace(value.Name) == "" || len(value.Name) > 100 || len(value.Description) > 2000 {
		return failure(400, "extension_manifest", "Manifest needs a reverse-domain ID, a name up to 100 characters and a description up to 2000 characters.")
	}
	if _, err := semver.Parse(value.Version); err != nil {
		return failure(400, "extension_version", "Extension version must be a complete semantic version.")
	}
	if len(value.Actions) == 0 || len(value.Actions) > 32 || len(value.Views) > 16 || len(value.Permissions) > len(capabilities) {
		return failure(400, "extension_contributions", "Provide 1-32 actions, at most 16 views and only supported permissions.")
	}
	seen := map[string]bool{}
	for _, permission := range value.Permissions {
		if !slices.Contains(capabilities, permission) || seen[permission] {
			return failure(400, "extension_permission", "Manifest permissions must be supported and unique.")
		}
		seen[permission] = true
	}
	seen = map[string]bool{}
	for _, action := range value.Actions {
		if !contributionID.MatchString(action.ID) || seen[action.ID] || action.Title == "" || len(action.Title) > 100 || (action.ResourceKind != "" && !contributionID.MatchString(action.ResourceKind)) {
			return failure(400, "extension_action", "Actions need unique IDs and titles up to 100 characters.")
		}
		seen[action.ID] = true
	}
	seen = map[string]bool{}
	for _, view := range value.Views {
		if !contributionID.MatchString(view.ID) || seen[view.ID] || view.Title == "" || len(view.Title) > 100 ||
			(view.Location != "tools" && view.Location != "resource") || (view.Location == "resource" && !contributionID.MatchString(view.ResourceKind)) || (view.Location == "tools" && view.ResourceKind != "") {
			return failure(400, "extension_view", "Views need unique IDs, titles and a tools or explicit resource location.")
		}
		seen[view.ID] = true
	}
	return nil
}

func (m *Manager) validateModule(ctx context.Context, module []byte) (wazero.CompiledModule, error) {
	compiled, err := m.runtime.CompileModule(ctx, module)
	if err != nil {
		return nil, failure(400, "extension_module", "The WebAssembly module is invalid, unsupported or exceeds its 64 MiB memory limit.")
	}
	reject := func(err error) (wazero.CompiledModule, error) {
		return nil, errors.Join(err, compiled.Close(context.Background()))
	}
	if ctx.Err() != nil {
		return reject(failure(408, "extension_compile_timeout", "Extension preparation exceeded its 30-second budget; no code or broker action was executed."))
	}
	if compiled.ExportedFunctions()["_start"] == nil {
		return reject(failure(400, "extension_module", "A WASI preview 1 module exporting _start is required."))
	}
	for _, fn := range compiled.ImportedFunctions() {
		module, _, _ := fn.Import()
		if module != "wasi_snapshot_preview1" {
			return reject(failure(403, "extension_import", "Only WASI preview 1 imports are supported. Host, Node, Electron and arbitrary native imports are not linked."))
		}
	}
	if len(compiled.ImportedMemories()) != 0 {
		return reject(failure(403, "extension_memory", "Shared/imported host memory is not allowed."))
	}
	return compiled, nil
}

func (m *Manager) Install(request InstallRequest) (Installed, error) {
	p := request.Package
	if err := validateManifest(p.Content.Manifest); err != nil {
		return Installed{}, err
	}
	if len(p.Content.UI) > 512<<10 || len(p.Content.Module) == 0 || len(p.Content.Module) > 8<<20 {
		return Installed{}, failure(400, "extension_package", "UI is limited to 512 KiB and WebAssembly modules to 8 MiB.")
	}
	hash := Digest(p.Content)
	if !digestPattern.MatchString(request.ExpectedSHA256) || hash != request.ExpectedSHA256 || hash != p.SHA256 {
		return Installed{}, failure(400, "extension_integrity", "Artifact SHA-256 does not match the explicitly approved digest. Nothing was installed or updated.")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Installed{}, failure(503, "extension_shutdown", "Extension host is shutting down.")
	}
	source := "developer"
	if p.PublicKey != "" || p.Signature != "" {
		if m.state.Publishers[p.PublicKey] == "" {
			return Installed{}, failure(403, "extension_publisher", "Publisher is not trusted. Verify its public key out of band and explicitly trust it before installation.")
		}
		if err := verifySignature(p.PublicKey, p.Signature, []byte(hash)); err != nil {
			return Installed{}, err
		}
		source = "trusted-publisher"
	} else if !request.Developer {
		return Installed{}, failure(403, "extension_developer", "Unsigned local packages require explicit developer-install consent and an independently reviewed digest.")
	}
	previous, exists := m.state.Installed[p.Content.Manifest.ID]
	if exists {
		oldVersion, err := semver.Parse(previous.Version)
		if err != nil {
			return Installed{}, err
		}
		version, _ := semver.Parse(p.Content.Manifest.Version)
		if !version.GT(oldVersion) || previous.Publisher != p.PublicKey || previous.Source != source {
			return Installed{}, failure(409, "extension_provenance", "Updates must increase the semantic version and retain the same developer/trusted-publisher identity. Remove explicitly before changing provenance.")
		}
	} else if len(m.state.Installed) >= 64 {
		return Installed{}, failure(429, "extension_limit", "At most 64 extensions can be installed.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	compiled, err := m.validateModule(ctx, p.Content.Module)
	if err != nil {
		return Installed{}, err
	}
	committed := false
	defer func() {
		if !committed {
			if err := compiled.Close(context.Background()); err != nil {
				slog.Warn("Release rejected extension module", "error", err)
			}
		}
	}()
	data, err := json.Marshal(p)
	if err != nil {
		return Installed{}, err
	}
	artifact := filepath.Join(m.root, "packages", hash+".json")
	if err := config.WriteFileAtomic(artifact, data, 0o600); err != nil {
		return Installed{}, err
	}
	value := Installed{ID: p.Content.Manifest.ID, Name: p.Content.Manifest.Name, Version: p.Content.Manifest.Version, SHA256: hash, Publisher: p.PublicKey, Source: source, Compatible: true, Generation: previous.Generation + 1, Manifest: p.Content.Manifest, State: "disabled", SecretNames: previous.SecretNames}
	m.state.Installed[value.ID] = value
	if err := m.saveLocked(); err != nil {
		if exists {
			m.state.Installed[value.ID] = previous
		} else {
			delete(m.state.Installed, value.ID)
		}
		return Installed{}, errors.Join(err, os.Remove(artifact))
	}
	m.cancelLocked(value.ID)
	m.compiled[hash] = compiled
	committed = true
	if previousModule := m.compiled[previous.SHA256]; previousModule != nil && previous.SHA256 != hash {
		delete(m.compiled, previous.SHA256)
		if err := previousModule.Close(context.Background()); err != nil {
			return cloneInstalled(value), err
		}
	}
	return cloneInstalled(value), nil
}

func verifySignature(publicKey, signature string, data []byte) error {
	key, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return failure(400, "extension_signature", "Publisher key must be a base64 Ed25519 public key.")
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, data, sig) {
		return failure(403, "extension_signature", "Package/catalog signature does not match its publisher and content.")
	}
	return nil
}

func cloneInstalled(value Installed) Installed {
	data, _ := json.Marshal(value)
	var copy Installed
	if err := json.Unmarshal(data, &copy); err != nil {
		panic(err)
	}
	return copy
}
func (m *Manager) List() []Installed {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := []Installed{}
	for _, value := range m.state.Installed {
		values = append(values, cloneInstalled(value))
	}
	slices.SortFunc(values, func(a, b Installed) int { return strings.Compare(a.ID, b.ID) })
	return values
}

func (m *Manager) Configure(id string, request Configuration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.state.Installed[id]
	if !ok {
		return failure(404, "extension_missing", "Extension is not installed.")
	}
	if request.Enabled && !request.Approve {
		return failure(403, "extension_approval", "Explicit approval of the selected capabilities and scopes is required to enable an extension.")
	}
	if err := validateGrants(value.Manifest, request.Grants); err != nil {
		return err
	}
	value.Enabled, value.Approved, value.Grants = request.Enabled, request.Enabled && request.Approve, request.Grants
	value.Generation++
	value.State, value.LastError = "disabled", ""
	if value.Enabled {
		value.State = "ready"
	}
	m.state.Installed[id] = value
	if err := m.saveLocked(); err != nil {
		value.Enabled, value.Approved, value.State, value.LastError = false, false, "error", "Grant persistence failed. This extension is disabled in this process; repair storage before reapproving it."
		m.state.Installed[id] = value
		m.cancelLocked(id)
		return err
	}
	m.cancelLocked(id)
	return nil
}

func (m *Manager) cancelLocked(id string) {
	for _, active := range m.active {
		if active.extension == id {
			active.cancel()
		}
	}
}
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.state.Installed[id]; !ok {
		return failure(404, "extension_missing", "Extension is not installed.")
	}
	m.cancelLocked(id)
	return nil
}

func (m *Manager) packageLocked(value Installed) (Package, error) {
	var p Package
	data, err := os.ReadFile(filepath.Join(m.root, "packages", value.SHA256+".json"))
	if err != nil {
		return p, err
	}
	if len(data) > MaxPackageBytes {
		return p, failure(400, "extension_integrity", "Installed artifact exceeds its size bound.")
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return p, failure(400, "extension_integrity", "Installed artifact is invalid; reinstall a verified package.")
	}
	if Digest(p.Content) != value.SHA256 || p.SHA256 != value.SHA256 || p.Content.Manifest.ID != value.ID || p.PublicKey != value.Publisher {
		return p, failure(403, "extension_integrity", "Installed artifact changed. Execution and UI loading were blocked; reinstall a verified package.")
	}
	if value.Publisher != "" {
		if m.state.Publishers[value.Publisher] == "" {
			return p, failure(403, "extension_publisher", "Publisher trust was revoked.")
		}
		if err := verifySignature(p.PublicKey, p.Signature, []byte(p.SHA256)); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (m *Manager) UI(id string, generation uint64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.state.Installed[id]
	if !ok || !value.Enabled || !value.Approved || value.Generation != generation {
		return "", failure(403, "extension_disabled", "This extension view is disabled, unapproved or stale. Refresh its installation and grants.")
	}
	p, err := m.packageLocked(value)
	if err != nil {
		return "", err
	}
	return p.Content.UI, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	if len(value) > b.limit-b.Len() {
		return 0, errors.New("extension output limit exceeded")
	}
	return b.Buffer.Write(value)
}

func (m *Manager) Invoke(parent context.Context, id string, request Invocation) (Result, error) {
	if len(request.Args) > 64<<10 || request.EnvironmentID == "" || len(request.EnvironmentID) > 128 {
		return Result{}, failure(400, "extension_invocation", "An explicit environment ID and at most 64 KiB of arguments are required.")
	}
	m.mu.Lock()
	value, ok := m.state.Installed[id]
	if !ok || m.closed || !value.Enabled || !value.Approved {
		m.mu.Unlock()
		return Result{}, failure(403, "extension_disabled", "The extension is disabled or lacks explicit approval.")
	}
	var action *Action
	for i := range value.Manifest.Actions {
		if value.Manifest.Actions[i].ID == request.Action {
			action = &value.Manifest.Actions[i]
			break
		}
	}
	if action == nil || (action.ResourceKind != "" && (request.ResourceKind != action.ResourceKind || request.ResourceID == "")) {
		m.mu.Unlock()
		return Result{}, failure(400, "extension_action", "Select a declared action and its explicit resource target.")
	}
	if action.Destructive && !request.Confirm {
		m.mu.Unlock()
		return Result{}, failure(403, "extension_confirmation", "This destructive action requires explicit confirmation of its selected target.")
	}
	p, err := m.packageLocked(value)
	if err != nil {
		m.mu.Unlock()
		return Result{}, err
	}
	compiled := m.compiled[value.SHA256]
	if compiled == nil {
		compileCtx, cancelCompile := context.WithTimeout(parent, 30*time.Second)
		compiled, err = m.validateModule(compileCtx, p.Content.Module)
		cancelCompile()
		if err != nil {
			m.mu.Unlock()
			return Result{}, err
		}
		m.compiled[value.SHA256] = compiled
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	select {
	case m.slots <- struct{}{}:
	default:
		m.mu.Unlock()
		return Result{}, failure(429, "extension_busy", "Two extension actions are already active. Wait or cancel one; actions are not queued.")
	}
	invocationID, err := environments.ID()
	if err != nil {
		<-m.slots
		m.mu.Unlock()
		return Result{}, err
	}
	job := &running{cancel: cancel, done: make(chan struct{}), extension: id}
	m.active[invocationID] = job
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.active, invocationID); close(job.done); m.mu.Unlock(); <-m.slots }()
	result, err := m.execute(ctx, value, compiled, request)
	m.mu.Lock()
	current, present := m.state.Installed[id]
	if present && current.Generation == value.Generation {
		current.LastRun, current.LastError = time.Now().UTC(), ""
		current.State = "ready"
		if err != nil {
			current.State, current.LastError = "error", err.Error()
		}
		m.state.Installed[id] = current
		if saveErr := m.saveLocked(); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
	}
	m.mu.Unlock()
	if err != nil {
		slog.Warn("Extension action failed", "extension", id, "action", request.Action, "error", err)
	}
	return result, err
}

func (m *Manager) execute(ctx context.Context, value Installed, compiled wazero.CompiledModule, request Invocation) (Result, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return Result{}, err
	}
	stdout, stderr := &limitedBuffer{limit: 64 << 10}, &limitedBuffer{limit: 16 << 10}
	module, err := m.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("").WithArgs(value.ID).WithStdin(bytes.NewReader(data)).WithStdout(stdout).WithStderr(stderr).WithSysNanotime().WithSysWalltime())
	if module != nil {
		defer module.Close(context.Background())
	}
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, failure(408, "extension_canceled", "Extension action canceled or timed out. Its module and broker handles were closed; inspect any explicitly requested side effects before retrying.")
		}
		return Result{}, failure(422, "extension_crash", "Extension module crashed, attempted an unavailable host import or exceeded resource/output bounds. No broker request was executed.")
	}
	var operation brokerRequest
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&operation); err != nil {
		return Result{}, failure(422, "extension_protocol", "The module must emit exactly one supported JSON broker request on stdout.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Result{}, failure(422, "extension_protocol", "Multiple or trailing broker requests are not supported.")
	}
	m.mu.Lock()
	current, ok := m.state.Installed[value.ID]
	authorized := ok && current.Enabled && current.Approved && current.Generation == value.Generation
	m.mu.Unlock()
	if !authorized || ctx.Err() != nil {
		return Result{}, failure(403, "extension_revoked", "Extension authority changed during execution. No new broker action was admitted.")
	}
	result, err := m.dispatch(ctx, value, request, operation)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, failure(408, "extension_canceled", "The action was canceled. Inspect the selected target before retrying an unconfirmed side effect.")
	}
	return Result{request.EnvironmentID, request.Action, result}, nil
}

func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.state.Installed[id]
	if !ok {
		return failure(404, "extension_missing", "Extension is not installed.")
	}
	value.Enabled, value.Approved, value.State = false, false, "disabled"
	value.Generation++
	m.state.Installed[id] = value
	m.cancelLocked(id)
	if err := m.saveLocked(); err != nil {
		return err
	}
	if compiled := m.compiled[value.SHA256]; compiled != nil {
		delete(m.compiled, value.SHA256)
		if err := compiled.Close(context.Background()); err != nil {
			return err
		}
	}
	for _, name := range value.SecretNames {
		if err := deleteSecret(m.vault, "dev.mbianchi.porto.extensions", m.secretAccount(id, name)); err != nil {
			return fmt.Errorf("remove extension-owned OS credential: %w", err)
		}
	}
	delete(m.state.Installed, id)
	if err := m.saveLocked(); err != nil {
		m.state.Installed[id] = value
		return err
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "packages"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		hash := strings.TrimSuffix(entry.Name(), ".json")
		if !digestPattern.MatchString(hash) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.root, "packages", entry.Name()))
		if err != nil {
			return err
		}
		var p Package
		if json.Unmarshal(data, &p) == nil && p.Content.Manifest.ID == id {
			if err := os.Remove(filepath.Join(m.root, "packages", entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	jobs := make([]*running, 0, len(m.active))
	for _, job := range m.active {
		job.cancel()
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		select {
		case <-job.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.runtime.Close(ctx)
}
