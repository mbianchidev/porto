package delivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mbianchidev/porto/internal/config"
	"github.com/mbianchidev/porto/internal/operations"
)

func (m *Manager) queueLocked(ctx context.Context, current Application, action string, work func(context.Context, string, func(string) error) error) (operations.Operation, error) {
	if m.preparing[current.ProjectID] {
		return operations.Operation{}, ErrConflict
	}
	var operationID string
	op, err := m.operations.Start(ctx, resourceKey(current.ProjectID), action, func(ctx context.Context, progress func(string) error) error {
		m.mu.Lock()
		id := operationID
		m.mu.Unlock()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return work(ctx, id, progress)
	})
	if err != nil {
		return op, err
	}
	operationID = op.ID
	current.LastOperation = op.ID
	if err := m.save(current); err != nil {
		cancelErr := m.operations.Cancel(op.ID)
		return op, errors.Join(err, cancelErr)
	}
	return op, nil
}

func (m *Manager) Rollback(ctx context.Context, id int64, revisionID string) (operations.Operation, error) {
	preview, err := m.PreviewRollback(ctx, id, revisionID)
	if err != nil {
		return operations.Operation{}, err
	}
	return m.Deploy(ctx, id, preview.Token)
}

func (m *Manager) Reconcile(ctx context.Context, id int64, trigger string) (operations.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.getWithoutWebhook(id)
	if err != nil {
		return operations.Operation{}, err
	}
	return m.reconcileLocked(ctx, current, trigger)
}

func (m *Manager) reconcileLocked(ctx context.Context, current Application, trigger string) (operations.Operation, error) {
	if current.Source.Kind != "git" || current.CurrentRevision == "" || trigger == "poll" && current.PollSeconds == 0 || trigger == "webhook" && !current.WebhookEnabled {
		return operations.Operation{}, fmt.Errorf("%w: selected Git update mode is not enabled", ErrConflict)
	}
	return m.queueLocked(ctx, current, "compose."+trigger, func(ctx context.Context, operationID string, progress func(string) error) error {
		current, err := m.Get(current.ProjectID)
		if err != nil {
			return err
		}
		if current.Drift {
			err := operations.Failure("Local deployment drift blocks Git updates. Review the current definition, explicitly detach Git ownership, or choose a recorded rollback.")
			return m.recordFailure(current, err)
		}
		if err := progress("fetching and validating the explicitly selected Git ref"); err != nil {
			return err
		}
		selected, err := m.loadDefinition(current.CurrentRevision)
		if err != nil {
			return m.recordFailure(current, err)
		}
		project, err := m.store.GetProjectByID(ctx, current.ProjectID)
		if err != nil {
			return m.recordFailure(current, err)
		}
		request := selected.Request
		request.Source, request.PollSeconds, request.WebhookEnabled = current.Source, current.PollSeconds, current.WebhookEnabled
		prepared, err := m.prepare(ctx, project, current, request)
		if err != nil {
			return m.recordFailure(current, err)
		}
		if selected.Commit == prepared.Commit {
			current.Failures, current.LastError = 0, ""
			if current.PollSeconds > 0 {
				current.NextPollAt = time.Now().UTC().Add(time.Duration(current.PollSeconds) * time.Second).Format(time.RFC3339Nano)
			}
			if err := m.save(current); err != nil {
				return err
			}
			return progress("selected Git commit is already deployed; no services changed")
		}
		return m.execute(ctx, current.ProjectID, prepared, operationID, "", progress)
	})
}

func (m *Manager) recordFailure(current Application, result error) error {
	current.Failures++
	current.LastError = operations.PublicMessage(result)
	delay := min(3600, max(60, current.PollSeconds)*(1<<min(current.Failures, 5)))
	current.NextPollAt = time.Now().UTC().Add(time.Duration(delay) * time.Second).Format(time.RFC3339Nano)
	return errors.Join(result, m.save(current))
}

func (m *Manager) ConfigureAutomation(id int64, pollSeconds int, webhook bool) (Application, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.operations.Busy(resourceKey(id)) || m.preparing[id] {
		return Application{}, ErrConflict
	}
	current, err := m.getWithoutWebhook(id)
	if err != nil {
		return current, err
	}
	if current.Source.Kind != "git" || current.CurrentRevision == "" {
		return current, fmt.Errorf("%w: automatic updates require a deployed Git-owned source", ErrInvalid)
	}
	if pollSeconds != 0 && (pollSeconds < 60 || pollSeconds > 86400) {
		return current, fmt.Errorf("%w: polling must be disabled or 60-86400 seconds", ErrInvalid)
	}
	if webhook {
		if _, err := m.vault.Get("webhook-" + strconv.FormatInt(id, 10)); err != nil {
			return current, errors.New("an available stored webhook secret is required")
		}
	}
	current.PollSeconds, current.WebhookEnabled = pollSeconds, webhook
	if pollSeconds > 0 {
		current.NextPollAt = time.Now().UTC().Add(time.Duration(pollSeconds) * time.Second).Format(time.RFC3339Nano)
	} else {
		current.NextPollAt = ""
	}
	return current, m.save(current)
}

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		values, err := m.List()
		if err != nil {
			slog.Error("Unable to read application polling configuration", "error", err)
			continue
		}
		for _, current := range values {
			if current.Source.Kind != "git" || current.PollSeconds == 0 {
				continue
			}
			next, err := time.Parse(time.RFC3339Nano, current.NextPollAt)
			if err != nil {
				slog.Warn("Application poll time is invalid", "project", current.ProjectID)
				continue
			}
			if time.Now().Before(next) {
				continue
			}
			if _, err := m.Reconcile(ctx, current.ProjectID, "poll"); err != nil && !errors.Is(err, operations.ErrBusy) && !errors.Is(err, ErrConflict) {
				slog.Warn("Unable to queue application update", "project", current.ProjectID, "reason", operations.PublicMessage(err))
			}
		}
	}
}

func (m *Manager) SetWebhookAddress(address string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.webhookAddress = address
}

var deliveryID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

func (m *Manager) WebhookHandler(base context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "unknown hook", http.StatusNotFound)
			return
		}
		current, err := m.Get(id)
		if err != nil || !current.WebhookEnabled || current.Source.Kind != "git" {
			http.Error(w, "unknown hook", http.StatusNotFound)
			return
		}
		timestamp, err := strconv.ParseInt(r.Header.Get("X-Porto-Timestamp"), 10, 64)
		now := time.Now().Unix()
		nonce := r.Header.Get("X-Porto-Delivery")
		if err != nil || timestamp < now-300 || timestamp > now+300 || !deliveryID.MatchString(nonce) {
			http.Error(w, "invalid webhook timestamp or delivery identity", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
		if err != nil {
			http.Error(w, "invalid bounded webhook payload", http.StatusBadRequest)
			return
		}
		secret, err := m.vault.Get("webhook-" + strconv.FormatInt(id, 10))
		if err != nil {
			http.Error(w, "webhook credential is unavailable", http.StatusServiceUnavailable)
			return
		}
		signature, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Porto-Signature"), "sha256="))
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(r.Header.Get("X-Porto-Timestamp") + "\n" + nonce + "\n"))
		_, _ = mac.Write(body)
		if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
			http.Error(w, "webhook signature is invalid", http.StatusUnauthorized)
			return
		}
		var payload struct {
			Repository string `json:"repository"`
			Ref        string `json:"ref"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.Repository != current.Source.Git.Repository || payload.Ref != current.Source.Git.Ref {
			http.Error(w, "webhook source does not match the approved repository/ref", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		current, err = m.getWithoutWebhook(id)
		if err != nil || !current.WebhookEnabled || payload.Repository != current.Source.Git.Repository || payload.Ref != current.Source.Git.Ref {
			http.Error(w, "webhook source changed", http.StatusConflict)
			return
		}
		path := filepath.Join(m.root, "replays", strconv.FormatInt(id, 10)+".json")
		seen := map[string]int64{}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, "webhook replay storage is unavailable", http.StatusServiceUnavailable)
			return
		}
		if len(data) > 0 && json.Unmarshal(data, &seen) != nil {
			http.Error(w, "webhook replay storage is invalid", http.StatusServiceUnavailable)
			return
		}
		if _, exists := seen[nonce]; exists {
			http.Error(w, "webhook delivery was already accepted", http.StatusConflict)
			return
		}
		for key, accepted := range seen {
			if accepted < now-600 {
				delete(seen, key)
			}
		}
		if len(seen) >= 1024 {
			http.Error(w, "webhook replay window is full", http.StatusTooManyRequests)
			return
		}
		op, err := m.reconcileLocked(base, current, "webhook")
		if err != nil {
			http.Error(w, operations.PublicMessage(err), http.StatusConflict)
			return
		}
		seen[nonce] = now
		data, err = json.Marshal(seen)
		if err == nil {
			err = config.WriteFileAtomic(path, data, 0o600)
		}
		if err != nil {
			cancelErr := m.operations.Cancel(op.ID)
			if cancelErr != nil {
				slog.Error("Unable to cancel unrecorded webhook admission", "operation", op.ID)
			}
			http.Error(w, "webhook admission could not be persisted", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(op)
	})
	return mux
}
