package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

func (m *Manager) rememberPreview(preview Preview, selected definition, runtimeDigest, rollbackOf string) (Preview, error) {
	data, err := json.Marshal(selected)
	if err != nil {
		return Preview{}, err
	}
	account := "preview-" + preview.Token
	if err := m.vault.Set(account, data); err != nil {
		return Preview{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.getWithoutWebhook(preview.ProjectID)
	if err != nil {
		return Preview{}, err
	}
	for id, existing := range m.previews {
		expiry, _ := time.Parse(time.RFC3339Nano, existing.Preview.ExpiresAt)
		if time.Now().After(expiry) {
			if err := m.vault.Delete(existing.Definition); err != nil {
				return Preview{}, err
			}
			delete(m.previews, id)
		}
	}
	if len(m.previews) >= 16 {
		return Preview{}, errors.Join(fmt.Errorf("%w: too many pending previews; wait for expiration", ErrConflict), m.vault.Delete(account))
	}
	m.previews[preview.Token] = pending{Preview: preview, Definition: account, RuntimeDigest: runtimeDigest, RollbackOf: rollbackOf, BaseIdentity: applicationIdentity(current)}
	return preview, nil
}

func applicationIdentity(value Application) string {
	data, _ := json.Marshal(struct {
		Revision    string
		Source      Source
		PollSeconds int
		Webhook     bool
	}{value.CurrentRevision, value.Source, value.PollSeconds, value.WebhookEnabled})
	return digestBytes(data)
}

func (m *Manager) PreviewRollback(ctx context.Context, id int64, revisionID string) (Preview, error) {
	m.mu.Lock()
	if m.preparing[id] || m.operations.Busy(resourceKey(id)) {
		m.mu.Unlock()
		return Preview{}, ErrConflict
	}
	m.preparing[id] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.preparing, id); m.mu.Unlock() }()
	current, err := m.Get(id)
	if err != nil {
		return Preview{}, err
	}
	found := false
	for _, revision := range current.Revisions {
		if revision.ID == revisionID {
			found = true
		}
	}
	if !found {
		return Preview{}, fmt.Errorf("%w: revision does not belong to this application", ErrInvalid)
	}
	selected, err := m.loadDefinition(revisionID)
	if err != nil {
		return Preview{}, err
	}
	project, err := m.store.GetProjectByID(ctx, id)
	if err != nil {
		return Preview{}, err
	}
	if _, err := m.backend.Validate(ctx, project, project.Path, selected.Effective, nil); err != nil {
		return Preview{}, err
	}
	selected.Request.PollSeconds, selected.Request.WebhookEnabled = 0, false
	m.registerRedactions(id, selected.Request.Secrets)
	runtime, err := os.ReadFile(m.currentPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Preview{}, err
	}
	token, err := randomID()
	if err != nil {
		return Preview{}, err
	}
	preview := Preview{Token: token, ProjectID: id, BaseRevision: current.CurrentRevision, Commit: selected.Commit, Before: m.Redact(id, publicConfiguration(runtime)), After: m.Redact(id, publicConfiguration(selected.Effective)), Warnings: []string{"Rollback restores the recorded definition and available immutable images, not database schemas or persistent data. Automatic polling and webhooks will be paused."}, ExpiresAt: time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)}
	return m.rememberPreview(preview, selected, digestBytes(runtime), revisionID)
}
