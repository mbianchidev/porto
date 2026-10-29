package delivery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/compose"
	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/credentials"
	"github.com/mbianchidev/porto/internal/deliverysource"
	"github.com/mbianchidev/porto/internal/localtools"
	"github.com/mbianchidev/porto/internal/networking"
	"github.com/mbianchidev/porto/internal/operations"
)

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
var applicationName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

type Manager struct {
	root           string
	store          ProjectStore
	backend        Backend
	operations     *operations.Manager
	vault          *credentials.EncryptedFiles
	runner         localtools.Runner
	mu             sync.Mutex
	previews       map[string]pending
	preparing      map[int64]bool
	redactions     map[int64][]string
	webhookAddress string
}

func New(root string, store ProjectStore, backend Backend, ops *operations.Manager, vault credentials.Store, runner localtools.Runner) *Manager {
	if runner == nil {
		runner = localtools.ExecRunner{}
	}
	return &Manager{root: root, store: store, backend: backend, operations: ops, vault: credentials.NewEncryptedFiles(filepath.Join(root, "sealed"), "dev.mbianchi.porto.delivery", vault), runner: runner, previews: map[string]pending{}, preparing: map[int64]bool{}, redactions: map[int64][]string{}}
}

func randomID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func resourceKey(id int64) string { return "project:" + strconv.FormatInt(id, 10) }
func (m *Manager) recordPath(id int64) string {
	return filepath.Join(m.root, "applications", strconv.FormatInt(id, 10)+".json")
}
func (m *Manager) currentPath(id int64) string {
	return filepath.Join(m.root, "runtime", strconv.FormatInt(id, 10)+".json")
}

func (m *Manager) save(value Application) error {
	value.WebhookURL = ""
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(m.recordPath(value.ProjectID), data, 0o600)
}

func (m *Manager) Get(id int64) (Application, error) {
	if id <= 0 {
		return Application{}, fmt.Errorf("%w: positive project ID required", ErrInvalid)
	}
	var value Application
	data, err := os.ReadFile(m.recordPath(id))
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	if value.CurrentRevision != "" {
		current, err := os.ReadFile(m.currentPath(id))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return value, err
		}
		for _, revision := range value.Revisions {
			if revision.ID == value.CurrentRevision {
				value.Drift = err != nil || digestBytes(current) != revision.ConfigDigest
				break
			}
		}
	}
	m.mu.Lock()
	if value.WebhookEnabled && m.webhookAddress != "" {
		value.WebhookURL = "http://" + m.webhookAddress + "/hooks/" + strconv.FormatInt(id, 10)
	}
	m.mu.Unlock()
	return value, nil
}

func (m *Manager) List() ([]Application, error) {
	entries, err := os.ReadDir(filepath.Join(m.root, "applications"))
	if errors.Is(err, os.ErrNotExist) {
		return []Application{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := []Application{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
		if err != nil {
			return nil, err
		}
		value, err := m.Get(id)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	slices.SortFunc(values, func(a, b Application) int { return strings.Compare(a.Name, b.Name) })
	return values, nil
}

func (m *Manager) project(ctx context.Context, request Request) (app.Project, Application, error) {
	var project app.Project
	var current Application
	if request.Target != "" && request.Target != "local" {
		return project, current, fmt.Errorf("%w: only the local target is supported; remote targets require Porto's environment abstraction", ErrInvalid)
	}
	if request.ProjectID == 0 {
		if !applicationName.MatchString(request.Name) {
			return project, current, fmt.Errorf("%w: a simple application name is required", ErrInvalid)
		}
		id, err := randomID()
		if err != nil {
			return project, current, err
		}
		directory := filepath.Join(m.root, "projects", id)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return project, current, err
		}
		project = app.Project{Name: request.Name, Path: directory, Strategy: "compose", Command: "docker compose -f compose.yaml up"}
		projectID, err := m.store.UpsertProject(ctx, project)
		if err != nil {
			return project, current, err
		}
		request.ProjectID = projectID
	}
	project, err := m.store.GetProjectByID(ctx, request.ProjectID)
	if err != nil {
		return project, current, err
	}
	if project.Strategy != "compose" {
		return project, current, fmt.Errorf("%w: delivery can only adopt a Compose project, never a native process or managed Kubernetes cluster", ErrInvalid)
	}
	current, err = m.Get(project.ID)
	if errors.Is(err, os.ErrNotExist) {
		current = Application{ProjectID: project.ID, Name: project.Name, Target: "local", Source: request.Source, Revisions: []Revision{}}
		err = m.save(current)
	}
	return project, current, err
}

func (m *Manager) Preview(ctx context.Context, request Request) (Preview, error) {
	switch request.Source.Kind {
	case "git":
		if err := request.Source.Git.Validate(); err != nil {
			return Preview{}, fmt.Errorf("%w: %s", ErrInvalid, err)
		}
	case "editor", "upload":
		request.Source = Source{Kind: request.Source.Kind}
	case "template":
		request.Source.Git = deliverysource.Git{}
		if !templateName.MatchString(request.Source.Template) || request.Source.TemplateVersion < 1 {
			return Preview{}, fmt.Errorf("%w: a valid template name/version is required", ErrInvalid)
		}
	case "":
		if request.ProjectID <= 0 {
			return Preview{}, fmt.Errorf("%w: source kind is required", ErrInvalid)
		}
	default:
		return Preview{}, fmt.Errorf("%w: unknown source kind", ErrInvalid)
	}
	project, current, err := m.project(ctx, request)
	if err != nil {
		return Preview{}, err
	}
	m.mu.Lock()
	if m.preparing[project.ID] || m.operations.Busy(resourceKey(project.ID)) {
		m.mu.Unlock()
		return Preview{}, ErrConflict
	}
	m.preparing[project.ID] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.preparing, project.ID); m.mu.Unlock() }()
	if current.Drift && !request.ConfirmDrift {
		return Preview{}, fmt.Errorf("%w: deployment definition has external or partial changes; explicitly confirm replacement or choose rollback", ErrConflict)
	}
	runtime, err := os.ReadFile(m.currentPath(project.ID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Preview{}, err
	}
	runtimeDigest := digestBytes(runtime)
	request.ProjectID, request.Name, request.Target = project.ID, project.Name, "local"
	definition, err := m.prepare(ctx, project, current, request)
	if err != nil {
		return Preview{}, err
	}
	token, err := randomID()
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{Token: token, ProjectID: project.ID, BaseRevision: current.CurrentRevision, Commit: definition.Commit, After: m.Redact(project.ID, publicConfiguration(definition.Effective)), Before: "No recorded deployment.", Warnings: []string{"Applying may recreate selected Compose services. Persistent volumes are never deleted; application/database schema changes are not rolled back."}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	if current.CurrentRevision != "" {
		old, err := m.loadDefinition(current.CurrentRevision)
		if err != nil {
			return Preview{}, err
		}
		preview.Before = m.Redact(project.ID, publicConfiguration(old.Effective))
		if len(runtime) > 0 && current.Drift {
			preview.Before = m.Redact(project.ID, publicConfiguration(runtime))
		}
	}
	return m.rememberPreview(preview, definition, runtimeDigest, "")
}

func (m *Manager) prepare(ctx context.Context, project app.Project, current Application, request Request) (definition, error) {
	if request.Source.Kind == "" {
		request.Source = current.Source
	}
	if current.Source.Kind == "git" && request.Source.Kind != "git" {
		if !request.DetachGit {
			return definition{}, fmt.Errorf("%w: explicitly detach Git ownership before editing its Compose definition", ErrConflict)
		}
		request.PollSeconds, request.WebhookEnabled = 0, false
	}
	if request.PollSeconds != 0 && (request.PollSeconds < 60 || request.PollSeconds > 86400) {
		return definition{}, fmt.Errorf("%w: polling must be disabled or 60-86400 seconds", ErrInvalid)
	}
	if (request.PollSeconds != 0 || request.WebhookEnabled) && request.Source.Kind != "git" {
		return definition{}, fmt.Errorf("%w: automatic updates require a Git-owned source", ErrInvalid)
	}
	if current.CurrentRevision != "" {
		old, err := m.loadDefinition(current.CurrentRevision)
		if err != nil {
			return definition{}, err
		}
		m.registerRedactions(project.ID, old.Request.Secrets)
		if request.Variables == nil {
			request.Variables = old.Request.Variables
		}
		merged := map[string]string{}
		for key, value := range old.Request.Secrets {
			merged[key] = value
		}
		for key, value := range request.Secrets {
			merged[key] = value
		}
		request.Secrets = merged
		if request.Compose == "" && request.Source.Kind != "git" {
			request.Compose = old.Request.Compose
		}
	}
	environment := map[string]string{}
	for _, values := range []map[string]string{request.Variables, request.Secrets} {
		for key, value := range values {
			if !variableName.MatchString(key) || reservedVariable(key) || len(value) > 64*1024 || strings.ContainsRune(value, 0) {
				return definition{}, fmt.Errorf("%w: invalid variable name or value", ErrInvalid)
			}
			environment[key] = value
		}
	}
	result := definition{Request: request, Directory: project.Path}
	switch request.Source.Kind {
	case "editor", "upload":
	case "template":
		template, err := m.template(request.Source.Template, request.Source.TemplateVersion)
		if err != nil {
			return result, err
		}
		for _, name := range template.Required {
			if environment[name] == "" {
				return result, fmt.Errorf("%w: template variable %s is required", ErrInvalid, name)
			}
		}
		for _, name := range template.SecretNames {
			if request.Secrets[name] == "" {
				return result, fmt.Errorf("%w: template variable %s must be supplied as a secret", ErrInvalid, name)
			}
		}
		result.Request.Compose = template.Compose
	case "git":
		token, err := m.sourceToken(project.ID, request.Source.Git.Repository, request.GitToken)
		if err != nil {
			return result, err
		}
		checkout, err := deliverysource.Fetch(ctx, filepath.Join(m.root, "sources"), request.Source.Git, token, m.runner)
		if err != nil {
			return result, err
		}
		root, err := os.OpenRoot(checkout.Directory)
		if err != nil {
			return result, err
		}
		defer root.Close()
		file, err := root.Open(filepath.FromSlash(request.Source.Git.Path))
		if err != nil {
			return result, fmt.Errorf("%w: selected Git Compose file is unavailable", ErrInvalid)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return result, fmt.Errorf("%w: selected Git Compose file is unavailable or exceeds 1 MiB", ErrInvalid)
		}
		data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		if err != nil {
			return result, err
		}
		if len(data) > 1024*1024 {
			return result, fmt.Errorf("%w: selected Git Compose file exceeds 1 MiB", ErrInvalid)
		}
		result.Request.Compose, result.Directory, result.Commit = string(data), checkout.Directory, checkout.Commit
	default:
		return result, fmt.Errorf("%w: source must be editor, upload, template or git", ErrInvalid)
	}
	if len(result.Request.Compose) == 0 || len(result.Request.Compose) > 1024*1024 {
		return result, fmt.Errorf("%w: Compose definition must be 1 byte to 1 MiB", ErrInvalid)
	}
	if request.WebhookSecret != nil {
		if len(*request.WebhookSecret) < 32 || len(*request.WebhookSecret) > 256 {
			return result, fmt.Errorf("%w: webhook secret must be 32-256 bytes", ErrInvalid)
		}
	}
	if request.WebhookEnabled && request.WebhookSecret == nil {
		if _, err := m.vault.Get("webhook-" + strconv.FormatInt(project.ID, 10)); err != nil {
			return result, errors.New("store a webhook secret before enabling authenticated webhooks")
		}
	}
	effective, err := m.backend.Validate(ctx, project, result.Directory, []byte(result.Request.Compose), environment)
	if err != nil {
		return result, err
	}
	if !json.Valid(effective) {
		return result, errors.New("Compose validation did not return machine-readable effective configuration")
	}
	result.Effective = effective
	m.registerRedactions(project.ID, result.Request.Secrets)
	return result, nil
}

func (m *Manager) sourceToken(id int64, repository string, supplied *string) (string, error) {
	repositoryDigest := sha256.Sum256([]byte(repository))
	name := "git-" + strconv.FormatInt(id, 10) + "-" + hex.EncodeToString(repositoryDigest[:8])
	var stored struct{ Repository, Token string }
	if supplied != nil {
		if len(*supplied) > 4096 || strings.ContainsAny(*supplied, "\r\n\x00") {
			return "", fmt.Errorf("%w: invalid scoped Git credential", ErrInvalid)
		}
		return *supplied, nil
	}
	data, err := m.vault.Get(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return "", err
	}
	if stored.Repository != repository {
		return "", fmt.Errorf("%w: stored Git access is scoped to another source; explicitly provide the new source credential or an empty public credential", ErrConflict)
	}
	return stored.Token, nil
}

func (m *Manager) Deploy(ctx context.Context, projectID int64, token string) (operations.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	preview, ok := m.previews[token]
	if !ok || preview.Preview.ProjectID != projectID {
		return operations.Operation{}, fmt.Errorf("%w: a matching current preview is required", ErrConflict)
	}
	expiry, err := time.Parse(time.RFC3339Nano, preview.Preview.ExpiresAt)
	if err != nil || time.Now().After(expiry) {
		return operations.Operation{}, fmt.Errorf("%w: preview expired; preview again", ErrConflict)
	}
	current, err := m.getWithoutWebhook(projectID)
	if err != nil {
		return operations.Operation{}, err
	}
	if current.CurrentRevision != preview.Preview.BaseRevision {
		return operations.Operation{}, fmt.Errorf("%w: deployment changed after preview", ErrConflict)
	}
	if applicationIdentity(current) != preview.BaseIdentity {
		return operations.Operation{}, fmt.Errorf("%w: source or automatic-update policy changed after preview", ErrConflict)
	}
	runtime, readErr := os.ReadFile(m.currentPath(projectID))
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return operations.Operation{}, readErr
	}
	if digestBytes(runtime) != preview.RuntimeDigest {
		return operations.Operation{}, fmt.Errorf("%w: runtime definition changed after preview; preview again", ErrConflict)
	}
	data, err := m.vault.Get(preview.Definition)
	if err != nil {
		return operations.Operation{}, err
	}
	var selected definition
	if err := json.Unmarshal(data, &selected); err != nil {
		return operations.Operation{}, err
	}
	action := "compose.deploy"
	if preview.RollbackOf != "" {
		action = "compose.rollback"
	}
	op, err := m.queueLocked(ctx, current, action, func(ctx context.Context, id string, progress func(string) error) error {
		return m.execute(ctx, projectID, selected, id, preview.RollbackOf, progress)
	})
	if err != nil {
		return op, err
	}
	delete(m.previews, token)
	if err := m.vault.Delete(preview.Definition); err != nil {
		_ = m.operations.Cancel(op.ID)
		return op, err
	}
	return op, nil
}

func (m *Manager) getWithoutWebhook(id int64) (Application, error) {
	var value Application
	data, err := os.ReadFile(m.recordPath(id))
	if err != nil {
		return value, err
	}
	err = json.Unmarshal(data, &value)
	return value, err
}

func (m *Manager) execute(ctx context.Context, id int64, selected definition, operationID, rollbackOf string, progress func(string) error) (result error) {
	current, err := m.Get(id)
	if err != nil {
		return err
	}
	defer func() {
		if result == nil {
			return
		}
		current.Failures++
		current.LastError = operations.PublicMessage(result)
		delay := time.Duration(min(3600, max(60, current.PollSeconds)*(1<<min(current.Failures, 5)))) * time.Second
		current.NextPollAt = time.Now().UTC().Add(delay).Format(time.RFC3339Nano)
		if err := m.save(current); err != nil {
			result = errors.Join(result, err)
		}
	}()
	project, err := m.store.GetProjectByID(ctx, id)
	if err != nil {
		return err
	}
	if selected.Request.Source.Kind == "git" {
		if err := deliverysource.Verify(ctx, selected.Directory, selected.Commit, m.runner); err != nil {
			return err
		}
	}
	if err := progress("applying selected Compose definition and checking service health"); err != nil {
		return err
	}
	m.registerRedactions(id, selected.Request.Secrets)
	if err := m.backend.Apply(ctx, project, func() error { return config.WriteFileAtomic(m.currentPath(id), selected.Effective, 0o600) }, progress); err != nil {
		return operations.Partial(operations.PublicMessage(err) + " The last successful revision and all persistent volumes are retained; inspect services before retrying or rolling back.")
	}
	images, pinErr := m.backend.PinImages(ctx, project)
	warnings := []string{}
	if pinErr != nil {
		warnings = append(warnings, "Deployment is running, but immutable image references could not all be recorded; rollback image availability is not guaranteed.")
	}
	applied, err := m.backend.AppliedDefinition(ctx, project)
	if err != nil {
		return operations.Partial("Deployment was applied but its effective configuration could not be recorded. Inspect current services before retrying or rolling back.")
	}
	pinned, err := pinConfiguration(applied, images)
	if err != nil {
		return operations.Partial("Deployment was applied but its rollback configuration could not be recorded.")
	}
	snapshot := selected
	snapshot.Effective = pinned
	if selected.Request.GitToken != nil {
		repository := selected.Request.Source.Git.Repository
		repositoryDigest := sha256.Sum256([]byte(repository))
		credential, err := json.Marshal(struct{ Repository, Token string }{repository, *selected.Request.GitToken})
		if err != nil {
			return err
		}
		if err := m.vault.Set("git-"+strconv.FormatInt(id, 10)+"-"+hex.EncodeToString(repositoryDigest[:8]), credential); err != nil {
			return operations.Partial("Deployment was applied but the scoped Git credential could not be saved; automatic updates remain unchanged.")
		}
	}
	if selected.Request.WebhookSecret != nil {
		if err := m.vault.Set("webhook-"+strconv.FormatInt(id, 10), []byte(*selected.Request.WebhookSecret)); err != nil {
			return operations.Partial("Deployment was applied but its webhook credential could not be saved; automatic updates remain unchanged.")
		}
	}
	snapshot.Request.GitToken, snapshot.Request.WebhookSecret = nil, nil
	revisionID, err := randomID()
	if err != nil {
		return err
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := m.vault.Set("revision-"+revisionID, data); err != nil {
		return operations.Partial("Deployment was applied but its encrypted revision could not be saved; recover the credential store before retrying.")
	}
	revision := Revision{ID: revisionID, OperationID: operationID, Commit: selected.Commit, ConfigDigest: digestBytes(selected.Effective), EffectiveDigest: digestBytes(applied), Images: images, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), RollbackOf: rollbackOf, Warnings: warnings}
	current.Source, current.CurrentRevision, current.LastOperation = selected.Request.Source, revisionID, operationID
	current.PollSeconds, current.WebhookEnabled = selected.Request.PollSeconds, selected.Request.WebhookEnabled
	current.Failures, current.LastError, current.Drift = 0, "", false
	current.Revisions = append(current.Revisions, revision)
	if current.PollSeconds > 0 {
		current.NextPollAt = time.Now().UTC().Add(time.Duration(current.PollSeconds) * time.Second).Format(time.RFC3339Nano)
	} else {
		current.NextPollAt = ""
	}
	if err := m.save(current); err != nil {
		return operations.Partial("Deployment was applied but its application history could not be saved; inspect current services and local storage.")
	}
	if pinErr != nil {
		return operations.Partial(warnings[0])
	}
	return nil
}

func (m *Manager) loadDefinition(revision string) (definition, error) {
	if !operations.ValidID(revision) {
		return definition{}, fmt.Errorf("%w: invalid revision identity", ErrInvalid)
	}
	data, err := m.vault.Get("revision-" + revision)
	if err != nil {
		return definition{}, err
	}
	var value definition
	err = json.Unmarshal(data, &value)
	return value, err
}

func (m *Manager) Definition(id int64) (Request, error) {
	current, err := m.Get(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Request{}, err
	}
	if current.CurrentRevision == "" {
		project, err := m.store.GetProjectByID(context.Background(), id)
		if err != nil {
			return Request{}, err
		}
		if project.Strategy != "compose" {
			return Request{}, fmt.Errorf("%w: only an existing Compose project can be adopted", ErrInvalid)
		}
		file, ok := compose.FindFile(project.Path)
		if !ok {
			return Request{}, fmt.Errorf("%w: this project has no recorded or local Compose definition", ErrInvalid)
		}
		path := filepath.Join(project.Path, file)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
			return Request{}, fmt.Errorf("%w: local Compose definition is unavailable or exceeds 1 MiB", ErrInvalid)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Request{}, err
		}
		return Request{ProjectID: id, Name: project.Name, Target: "local", Source: Source{Kind: "editor"}, Compose: string(data), Variables: map[string]string{}}, nil
	}
	value, err := m.loadDefinition(current.CurrentRevision)
	if err != nil {
		return Request{}, err
	}
	value.Request.Secrets = nil
	value.Request.GitToken, value.Request.WebhookSecret = nil, nil
	return value.Request, nil
}

func (m *Manager) DefinitionPath(project app.Project) (string, error) {
	current, err := m.Get(project.ID)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if current.CurrentRevision == "" {
		if _, err := os.Stat(m.currentPath(project.ID)); err == nil {
			return m.currentPath(project.ID), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		return "", nil
	}
	definition, err := m.loadDefinition(current.CurrentRevision)
	if err != nil {
		return "", err
	}
	m.registerRedactions(project.ID, definition.Request.Secrets)
	return m.currentPath(project.ID), nil
}

func (m *Manager) registerRedactions(id int64, values map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.redactions[id]; !exists {
		m.redactions[id] = []string{}
	}
	for _, value := range values {
		if value != "" && !slices.Contains(m.redactions[id], value) {
			m.redactions[id] = append(m.redactions[id], value)
		}
	}
	slices.SortFunc(m.redactions[id], func(a, b string) int { return len(b) - len(a) })
}

func (m *Manager) Redact(id int64, value string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	secrets, managed := m.redactions[id]
	if !managed {
		return value
	}
	for _, secret := range secrets {
		value = strings.ReplaceAll(value, secret, "[redacted]")
	}
	return networking.Redact(value)
}

func publicConfiguration(data []byte) string {
	var document struct {
		Services map[string]json.RawMessage `json:"services"`
		Volumes  map[string]json.RawMessage `json:"volumes"`
		Networks map[string]json.RawMessage `json:"networks"`
	}
	if json.Unmarshal(data, &document) != nil {
		return "Configuration is unavailable."
	}
	services := map[string]any{}
	for name, raw := range document.Services {
		var service map[string]json.RawMessage
		if json.Unmarshal(raw, &service) != nil {
			return "Configuration is unavailable."
		}
		public := map[string]json.RawMessage{}
		for _, key := range []string{"image", "platform", "ports", "cpus", "mem_limit", "deploy", "volumes", "networks", "profiles"} {
			if value := service[key]; value != nil {
				public[key] = value
			}
		}
		if service["environment"] != nil {
			public["environment"] = json.RawMessage(`"[values hidden; definition hash detects all changes]"`)
		}
		if service["build"] != nil {
			public["build"] = json.RawMessage(`"[build definition present; review trusted source before applying]"`)
		}
		services[name] = public
	}
	result, _ := json.MarshalIndent(map[string]any{"services": services, "volumeNames": sortedKeys(document.Volumes), "networkNames": sortedKeys(document.Networks), "effectiveDefinitionSHA256": digestBytes(data)}, "", "  ")
	return string(result)
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func pinConfiguration(data []byte, images map[string]string) ([]byte, error) {
	var configuration map[string]json.RawMessage
	if err := json.Unmarshal(data, &configuration); err != nil {
		return nil, err
	}
	var services map[string]map[string]json.RawMessage
	if err := json.Unmarshal(configuration["services"], &services); err != nil {
		return nil, err
	}
	for name, reference := range images {
		if service := services[name]; service != nil {
			image, err := json.Marshal(reference)
			if err != nil {
				return nil, err
			}
			service["image"] = image
			delete(service, "build")
			service["pull_policy"] = json.RawMessage(`"never"`)
		}
	}
	encoded, err := json.Marshal(services)
	if err != nil {
		return nil, err
	}
	configuration["services"] = encoded
	return json.Marshal(configuration)
}
