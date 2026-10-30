package delivery

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbianchidev/porto/internal/app"
	"github.com/mbianchidev/porto/internal/deliverysource"
	"github.com/mbianchidev/porto/internal/operations"
	"github.com/mbianchidev/porto/internal/store"
	"github.com/zalando/go-keyring"
	"gopkg.in/yaml.v3"
)

type fixtureVault struct {
	mu     sync.Mutex
	values map[string]string
}

func (v *fixtureVault) Set(service, account, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.values[service+"/"+account] = secret
	return nil
}
func (v *fixtureVault) Get(service, account string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[service+"/"+account]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (v *fixtureVault) Delete(service, account string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.values, service+"/"+account)
	return nil
}

type fixtureBackend struct {
	applies atomic.Int32
	fail    atomic.Bool
	block   atomic.Bool
	current func(int64) string
}

func (b *fixtureBackend) AppliedDefinition(_ context.Context, project app.Project) ([]byte, error) {
	return os.ReadFile(b.current(project.ID))
}

func (b *fixtureBackend) Validate(_ context.Context, _ app.Project, _ string, raw []byte, environment map[string]string, _ ...string) ([]byte, error) {
	var definition struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(raw, &definition) != nil || len(definition.Services) == 0 {
		return nil, operations.Failure("Invalid synthetic Compose definition.")
	}
	services := map[string]any{}
	for name, service := range definition.Services {
		image := service.Image
		if strings.HasPrefix(image, "${APP_IMAGE") {
			image = environment["APP_IMAGE"]
		}
		services[name] = map[string]any{"image": image, "environment": environment, "ports": []string{"127.0.0.1:8080:80"}}
	}
	return json.Marshal(map[string]any{"services": services, "volumes": map[string]any{"synthetic-data": map[string]any{}}})
}
func (b *fixtureBackend) Apply(ctx context.Context, _ app.Project, activate func() error, progress func(string) error) error {
	if err := progress("starting synthetic deployment"); err != nil {
		return err
	}
	if err := activate(); err != nil {
		return err
	}
	b.applies.Add(1)
	if b.block.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	if b.fail.Load() {
		return operations.Failure("Synthetic health check failed.")
	}
	return nil
}
func (b *fixtureBackend) PinImages(context.Context, app.Project) (map[string]string, error) {
	return map[string]string{"app": "registry.invalid/synthetic@sha256:" + strings.Repeat("a", 64)}, nil
}

func deliveryFixture(t *testing.T) (*Manager, *fixtureBackend, *operations.Manager) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "porto.db"))
	if err != nil {
		t.Fatal(err)
	}
	ops := operations.New(filepath.Join(root, "operations"))
	backend := &fixtureBackend{}
	manager := New(filepath.Join(root, "delivery"), st, backend, ops, &fixtureVault{values: map[string]string{}}, nil)
	backend.current = manager.currentPath
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ops.Close(ctx); err != nil {
			t.Error(err)
		}
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return manager, backend, ops
}

func awaitOperation(t *testing.T, ops *operations.Manager, id string) operations.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	value, err := ops.Wait(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func deployFixture(t *testing.T, manager *Manager, ops *operations.Manager, request Request) (Application, operations.Operation) {
	t.Helper()
	preview, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	op, err := manager.Deploy(context.Background(), preview.ProjectID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	result := awaitOperation(t, ops, op.ID)
	current, err := manager.Get(preview.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	return current, result
}

func TestSavedConfigurationReturnsOnlyNonSecretVariables(t *testing.T) {
	manager, _, ops := deliveryFixture(t)
	request := Request{Name: "synthetic-configuration", Source: Source{Kind: "editor"},
		Compose:   "services:\n  app:\n    image: registry.invalid/synthetic:one\n    environment:\n      INLINE: synthetic-inline-private-value\n",
		Variables: map[string]string{"APP_IMAGE": "registry.invalid/synthetic:saved", "SHADOWED": "synthetic-old-public-value"},
		Secrets:   map[string]string{"TOKEN": "synthetic-private-value", "SHADOWED": "synthetic-shadowed-private-value"}}
	current, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	settings, err := manager.Configuration(current.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Variables) != 1 || settings.Variables["APP_IMAGE"] != request.Variables["APP_IMAGE"] {
		t.Fatalf("saved non-secret variables were dropped or mixed with secret-backed keys: %+v", settings)
	}
	public, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic-inline-private-value", "synthetic-private-value", "synthetic-shadowed-private-value", "SHADOWED", "TOKEN", `"compose"`} {
		if bytes.Contains(public, []byte(private)) {
			t.Fatalf("configuration metadata revealed private source or credentials: %s", private)
		}
	}
	if err := manager.vault.Delete("revision-" + current.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Configuration(current.ProjectID); err == nil {
		t.Fatal("unavailable saved configuration became an empty variable map")
	}
}

func TestComposeProfileSelectionIsValidatedRecordedInheritedClearedAndRolledBack(t *testing.T) {
	manager, backend, ops := deliveryFixture(t)
	validation := &sourceDirectoryBackend{Backend: backend}
	manager.backend = validation
	request := Request{Name: "synthetic-profile-history", Source: Source{Kind: "editor"},
		Compose: "services:\n  app:\n    image: registry.invalid/synthetic:one\n", Profiles: []string{"tools", "dev", "tools"}}
	expected := []string{"dev", "tools"}
	preview, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(preview.Profiles, expected) || !slices.Equal(validation.profiles, expected) {
		t.Fatalf("profile selection was not bound to native validation and preview: %v / %v", preview.Profiles, validation.profiles)
	}
	op, err := manager.Deploy(context.Background(), preview.ProjectID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	current, err := manager.Get(preview.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	first := current.CurrentRevision
	settings, err := manager.Configuration(current.ProjectID)
	if err != nil || !slices.Equal(settings.Profiles, expected) || !slices.Equal(current.Profiles, expected) ||
		!slices.Equal(current.Revisions[0].Profiles, expected) {
		t.Fatalf("deployment forgot its profile selection: %+v / %+v / %v", settings, current, err)
	}
	request.ProjectID = current.ProjectID
	request.Profiles = nil
	inherited, err := manager.Preview(context.Background(), request)
	if err != nil || !slices.Equal(inherited.Profiles, expected) {
		t.Fatalf("omitted profiles silently reset a saved selection: %+v, %v", inherited.Profiles, err)
	}
	request.Profiles = []string{}
	current, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" || len(current.Profiles) != 0 || len(validation.profiles) != 0 {
		t.Fatalf("explicit empty profiles did not restore default selection: %+v, %+v", current, result)
	}
	rollback, err := manager.PreviewRollback(context.Background(), current.ProjectID, first)
	if err != nil || !slices.Equal(rollback.Profiles, expected) {
		t.Fatalf("rollback lost the retained profile selection: %+v, %v", rollback.Profiles, err)
	}
	op, err = manager.Deploy(context.Background(), current.ProjectID, rollback.Token)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	settings, err = manager.Configuration(current.ProjectID)
	if err != nil || !slices.Equal(settings.Profiles, expected) {
		t.Fatalf("rollback did not restore saved profiles: %+v, %v", settings, err)
	}
}

func TestEditorUploadTemplateAndRecordedRollbackPreserveSecretBoundaries(t *testing.T) {
	manager, backend, ops := deliveryFixture(t)
	request := Request{Name: "synthetic-app", Source: Source{Kind: "editor"}, Compose: "services:\n  app:\n    image: registry.invalid/synthetic:one\n", Secrets: map[string]string{"TOKEN": "synthetic-private-value"}}
	preview, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(preview)
	if bytes.Contains(public, []byte("synthetic-private-value")) {
		t.Fatal("secret leaked into a preview")
	}
	op, err := manager.Deploy(context.Background(), preview.ProjectID, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	if value := awaitOperation(t, ops, op.ID); value.Status != "succeeded" {
		t.Fatalf("deployment failed: %+v", value)
	}
	first, err := manager.Get(preview.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	request.ProjectID = first.ProjectID
	request.Source.Kind = "upload"
	request.Compose = "services:\n  app:\n    image: registry.invalid/synthetic:two\n"
	second, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" || len(second.Revisions) != 2 || second.CurrentRevision == first.CurrentRevision {
		t.Fatalf("update was not recorded: %+v", second)
	}
	rollback, err := manager.Rollback(context.Background(), first.ProjectID, first.CurrentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, rollback.ID); result.Status != "succeeded" {
		t.Fatalf("rollback failed: %+v", result)
	}
	current, err := manager.Get(first.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revisions[len(current.Revisions)-1].RollbackOf != first.CurrentRevision || backend.applies.Load() != 3 {
		t.Fatalf("wrong recorded rollback: %+v", current)
	}
	data, err := os.ReadFile(manager.currentPath(first.ProjectID))
	if err != nil || !bytes.Contains(data, []byte(`"pull_policy":"never"`)) {
		t.Fatal("rollback did not restore recorded local immutable image references")
	}
	records, _ := json.Marshal(current)
	history, _ := ops.List()
	operationJSON, _ := json.Marshal(history)
	if bytes.Contains(records, []byte("synthetic-private-value")) || bytes.Contains(operationJSON, []byte("synthetic-private-value")) {
		t.Fatal("secret leaked into application or operation history")
	}
	if strings.Contains(manager.Redact(first.ProjectID, "TOKEN=synthetic-private-value"), "synthetic-private-value") {
		t.Fatal("workload log redaction did not use recorded secret values")
	}
	template := Request{Name: "synthetic-template", Source: Source{Kind: "template", Template: "http-application", TemplateVersion: 1}, Variables: map[string]string{"APP_IMAGE": "registry.invalid/synthetic:template", "CONTAINER_PORT": "80"}}
	if _, result := deployFixture(t, manager, ops, template); result.Status != "succeeded" {
		t.Fatal(result)
	}
	template.Name = "missing-variable"
	delete(template.Variables, "APP_IMAGE")
	if _, err := manager.Preview(context.Background(), template); err == nil {
		t.Fatal("template accepted a missing required variable")
	}
}

func TestConcurrentEditsDriftCancellationAndFailedRollbackAreNotSuccess(t *testing.T) {
	manager, backend, ops := deliveryFixture(t)
	request := Request{Name: "synthetic-drift", Source: Source{Kind: "editor"}, Compose: "services:\n  app:\n    image: registry.invalid/synthetic:one\n"}
	current, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	request.ProjectID = current.ProjectID
	first, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.currentPath(current.ProjectID), []byte(`{"services":{"app":{"image":"registry.invalid/external:edit"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Deploy(context.Background(), current.ProjectID, first.Token); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent edit was overwritten: %v", err)
	}
	if _, err := manager.Preview(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatalf("drift did not block replacement: %v", err)
	}
	backend.fail.Store(true)
	rollback, err := manager.Rollback(context.Background(), current.ProjectID, current.CurrentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, rollback.ID); result.Status != "partial" {
		t.Fatalf("failed rollback looked successful: %+v", result)
	}
	after, err := manager.Get(current.ProjectID)
	if err != nil || after.CurrentRevision != current.CurrentRevision || after.LastError == "" {
		t.Fatalf("failure replaced last successful revision: %+v %v", after, err)
	}
	backend.fail.Store(false)
	backend.block.Store(true)
	rollback, err = manager.Rollback(context.Background(), current.ProjectID, current.CurrentRevision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(context.Background(), current.ProjectID, current.CurrentRevision); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent mutation was admitted: %v", err)
	}
	if err := ops.Cancel(rollback.ID); err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, rollback.ID); result.Status == "succeeded" {
		t.Fatal("canceled deployment looked successful")
	}
}

func gitFixture(t *testing.T) (string, func(string) string) {
	t.Helper()
	root := t.TempDir()
	run := func(arguments ...string) string {
		command := exec.Command("git", append([]string{"-C", root, "-c", "user.name=Synthetic Fixture", "-c", "user.email=synthetic@example.invalid", "-c", "commit.gpgsign=false"}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("synthetic Git: %s %v", output, err)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "--initial-branch=main")
	commit := func(image string) string {
		if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services:\n  app:\n    image: registry.invalid/synthetic:"+image+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", "compose.yaml")
		run("commit", "--no-gpg-sign", "-m", "Synthetic image revision")
		return run("rev-parse", "HEAD")
	}
	commit("one")
	return root, commit
}

type sourceDirectoryBackend struct {
	Backend
	directory string
	profiles  []string
}

func (b *sourceDirectoryBackend) Validate(ctx context.Context, project app.Project, directory string, raw []byte, variables map[string]string, profiles ...string) ([]byte, error) {
	b.directory = directory
	b.profiles = slices.Clone(profiles)
	return b.Backend.Validate(ctx, project, directory, raw, variables, profiles...)
}

func TestGitStackResolvesRelativeFilesFromItsComposeDirectory(t *testing.T) {
	manager, backend, _ := deliveryFixture(t)
	repository, _ := gitFixture(t)
	if err := os.Mkdir(filepath.Join(repository, "deploy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "deploy", "compose.yaml"), []byte("services:\n  app:\n    image: registry.invalid/synthetic:one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range [][]string{{"add", "deploy/compose.yaml"}, {"-c", "user.name=Synthetic Fixture", "-c", "user.email=synthetic@example.invalid", "commit", "--no-gpg-sign", "-m", "Synthetic nested stack"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("commit synthetic nested stack: %s %v", output, err)
		}
	}
	recorder := &sourceDirectoryBackend{Backend: backend}
	manager.backend = recorder
	preview, err := manager.Preview(context.Background(), Request{Name: "synthetic-nested-stack", Source: Source{Kind: "git", Git: deliverysource.Git{Repository: repository, Ref: "main", Path: "deploy/compose.yaml"}}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := manager.vault.Get(manager.previews[preview.Token].Definition)
	if err != nil {
		t.Fatal(err)
	}
	var selected definition
	if err := json.Unmarshal(stored, &selected); err != nil {
		t.Fatal(err)
	}
	if recorder.directory != filepath.Join(selected.Directory, "deploy") {
		t.Fatalf("Compose working directory = %q, want nested source %q", recorder.directory, filepath.Join(selected.Directory, "deploy"))
	}
	if err := deliverysource.Verify(context.Background(), selected.Directory, selected.Commit, manager.runner); err != nil {
		t.Fatalf("immutable verification no longer uses the complete repository: %v", err)
	}
}

func TestGitUpdatesAuthenticatedWebhooksAndDurableReplay(t *testing.T) {
	manager, backend, ops := deliveryFixture(t)
	repository, commit := gitFixture(t)
	secret := strings.Repeat("synthetic-hook-key-", 3)
	request := Request{Name: "synthetic-git", Source: Source{Kind: "git", Git: deliverysource.Git{Repository: repository, Ref: "main", Path: "compose.yaml"}}, WebhookEnabled: true, WebhookSecret: &secret}
	current, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	expected := commit("two")
	body, _ := json.Marshal(map[string]string{"repository": repository, "ref": "main"})
	handler := manager.WebhookHandler(context.Background())
	deliver := func(nonce string, authenticated bool) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/hooks/"+strconv.FormatInt(current.ProjectID, 10), bytes.NewReader(body))
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		request.Header.Set("X-Porto-Timestamp", timestamp)
		request.Header.Set("X-Porto-Delivery", nonce)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(timestamp + "\n" + nonce + "\n"))
		mac.Write(body)
		signature := hex.EncodeToString(mac.Sum(nil))
		if !authenticated {
			signature = strings.Repeat("0", 64)
		}
		request.Header.Set("X-Porto-Signature", "sha256="+signature)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := deliver("synthetic-denied", false); response.Code != http.StatusUnauthorized {
		t.Fatal(response)
	}
	response := deliver("synthetic-delivery-one", true)
	if response.Code != http.StatusAccepted {
		t.Fatalf("authorized webhook failed: %s", response.Body.String())
	}
	var op operations.Operation
	if err := json.Unmarshal(response.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatalf("Git update failed: %+v", result)
	}
	current, err := manager.Get(current.ProjectID)
	if err != nil || current.Revisions[len(current.Revisions)-1].Commit != expected || backend.applies.Load() != 2 {
		t.Fatalf("exact Git revision was not deployed once: %+v %v", current, err)
	}
	if response := deliver("synthetic-delivery-one", true); response.Code != http.StatusConflict {
		t.Fatal("webhook replay was accepted")
	}
	response = deliver("synthetic-delivery-two", true)
	if response.Code != http.StatusAccepted {
		t.Fatal(response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	awaitOperation(t, ops, op.ID)
	if backend.applies.Load() != 2 {
		t.Fatal("same Git commit redeployed twice")
	}
	restarted := New(manager.root, manager.store, backend, ops, nil, nil)
	restarted.vault = manager.vault
	handler = restarted.WebhookHandler(context.Background())
	if response := deliver("synthetic-delivery-one", true); response.Code != http.StatusConflict {
		t.Fatal("webhook replay protection was lost across manager restart")
	}
	request.ProjectID = current.ProjectID
	request.Source = Source{Kind: "editor"}
	request.Compose = "services:\n  app:\n    image: registry.invalid/synthetic:detached\n"
	if _, err := manager.Preview(context.Background(), request); !errors.Is(err, ErrConflict) {
		t.Fatal("manual edits silently overwrote Git ownership")
	}
	request.DetachGit = true
	detached, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" || detached.WebhookEnabled || detached.PollSeconds != 0 || detached.Source.Kind != "editor" {
		t.Fatalf("detachment retained automatic writers: %+v", detached)
	}
}

func TestPollingIsOptInAndApplicationPolicyChangesInvalidatePreviews(t *testing.T) {
	manager, backend, ops := deliveryFixture(t)
	repository, commit := gitFixture(t)
	request := Request{Name: "synthetic-poll", Source: Source{Kind: "git", Git: deliverysource.Git{Repository: repository, Ref: "main", Path: "compose.yaml"}}}
	current, result := deployFixture(t, manager, ops, request)
	if result.Status != "succeeded" {
		t.Fatal(result)
	}
	if _, err := manager.Reconcile(context.Background(), current.ProjectID, "poll"); !errors.Is(err, ErrConflict) {
		t.Fatal("polling ran without opt-in")
	}
	request.ProjectID = current.ProjectID
	preview, err := manager.Preview(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ConfigureAutomation(current.ProjectID, 60, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Deploy(context.Background(), current.ProjectID, preview.Token); !errors.Is(err, ErrConflict) {
		t.Fatal("stale preview overwrote a newly selected automatic-update policy")
	}
	expected := commit("two")
	op, err := manager.Reconcile(context.Background(), current.ProjectID, "poll")
	if err != nil {
		t.Fatal(err)
	}
	if result := awaitOperation(t, ops, op.ID); result.Status != "succeeded" {
		t.Fatal(result)
	}
	current, err = manager.Get(current.ProjectID)
	if err != nil || current.Revisions[len(current.Revisions)-1].Commit != expected || backend.applies.Load() != 2 {
		t.Fatalf("opted-in poll did not deploy the exact revision once: %+v %v", current, err)
	}
}
