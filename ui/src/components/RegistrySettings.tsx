import { useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { RegistryProfile, RegistryProvider } from '../types'

type RegistryDraft = {
  name: string
  provider: RegistryProvider
  server: string
  username: string
  credential: string
  testImage: string
  enabled: boolean
}

const PROVIDER_DEFAULTS: Record<RegistryProvider, { label: string; server: string; testImage: string }> = {
  'docker-hub': {
    label: 'Docker Hub',
    server: 'https://index.docker.io/v1/',
    testImage: 'alpine:latest',
  },
  github: {
    label: 'GitHub Container Registry',
    server: 'ghcr.io',
    testImage: '',
  },
  gitlab: {
    label: 'GitLab Container Registry',
    server: 'registry.gitlab.com',
    testImage: '',
  },
  custom: {
    label: 'Custom OCI registry',
    server: '',
    testImage: '',
  },
}

function emptyDraft(): RegistryDraft {
  return {
    name: 'Docker Hub',
    provider: 'docker-hub',
    server: PROVIDER_DEFAULTS['docker-hub'].server,
    username: '',
    credential: '',
    testImage: PROVIDER_DEFAULTS['docker-hub'].testImage,
    enabled: true,
  }
}

function profileDraft(profile: RegistryProfile): RegistryDraft {
  return {
    name: profile.name,
    provider: profile.provider,
    server: profile.server,
    username: profile.username,
    credential: '',
    testImage: profile.testImage,
    enabled: profile.enabled,
  }
}

function registryState(profile: RegistryProfile) {
  if (!profile.enabled) return { className: 'idle', label: 'disabled' }
  if (profile.verified) return { className: 'ready', label: 'verified' }
  if (profile.lastError) return { className: 'error', label: 'verification failed' }
  return { className: 'missing', label: 'verification required' }
}

function verifiedTime(value?: string) {
  if (!value) return 'Not verified yet'
  const date = new Date(value)
  return Number.isNaN(date.valueOf()) ? value : `Verified ${date.toLocaleString()}`
}

export function RegistrySettings() {
  const { notifyError, notifyNotice } = useMessages()
  const profiles = usePolledResource<RegistryProfile[]>(
    (signal) => apiGet('/api/registries', signal),
    0,
    [],
    'registries',
  )
  const [editingID, setEditingID] = useState<number | null>(null)
  const [draft, setDraft] = useState<RegistryDraft>(emptyDraft)
  const [saving, setSaving] = useState(false)
  const [verifyingID, setVerifyingID] = useState<number | null>(null)

  function reset() {
    setEditingID(null)
    setDraft(emptyDraft())
  }

  function selectProvider(provider: RegistryProvider) {
    const defaults = PROVIDER_DEFAULTS[provider]
    setDraft((current) => ({
      ...current,
      provider,
      name: current.name === '' || Object.values(PROVIDER_DEFAULTS).some((value) => value.label === current.name)
        ? defaults.label
        : current.name,
      server: defaults.server,
      testImage: defaults.testImage,
    }))
  }

  async function verify(profile: RegistryProfile) {
    setVerifyingID(profile.id)
    try {
      const verified = await apiSend<RegistryProfile>(`/api/registries/${profile.id}/verify`, 'POST')
      notifyNotice('registries', `${verified.name} verified by pulling ${verified.testImage}.`)
      profiles.reload()
      return true
    } catch (err) {
      notifyError('registries', errorMessage(err, `Unable to verify ${profile.name}`))
      profiles.reload()
      return false
    } finally {
      setVerifyingID(null)
    }
  }

  async function save(event: FormEvent) {
    event.preventDefault()
    setSaving(true)
    try {
      const saved = await apiSend<RegistryProfile>(
        editingID === null ? '/api/registries' : `/api/registries/${editingID}`,
        editingID === null ? 'POST' : 'PUT',
        draft,
      )
      profiles.reload()
      setEditingID(saved.id)
      setDraft(profileDraft(saved))
      const verified = await verify(saved)
      if (verified) reset()
    } catch (err) {
      notifyError('registries', errorMessage(err, `Unable to save ${draft.name || 'registry'}`))
    } finally {
      setSaving(false)
    }
  }

  async function remove(profile: RegistryProfile) {
    if (!window.confirm(`Remove ${profile.name} and its saved credential?`)) return
    try {
      await apiSend(`/api/registries/${profile.id}`, 'DELETE')
      if (editingID === profile.id) reset()
      notifyNotice('registries', `${profile.name} removed from Porto and the system credential store.`)
      profiles.reload()
    } catch (err) {
      notifyError('registries', errorMessage(err, `Unable to remove ${profile.name}`))
    }
  }

  const items = profiles.data ?? []
  const customServer = draft.provider === 'custom'

  return (
    <section className="integration registrySettings" aria-labelledby="registry-settings-title">
      <div className="hygieneIntro">
        <h2 id="registry-settings-title">Patch private registries into every runtime.</h2>
        <p>
          Porto keeps credentials in the system credential store. Verified profiles are matched by
          image host for pulls and synchronized into every managed cluster namespace.
        </p>
      </div>
      <div className="registryWorkbench">
        <div className="registryProfileList" aria-live="polite">
          {profiles.error && <p className="errorLine" role="alert">{profiles.error}</p>}
          {items.length === 0 && (
            <div className="registryEmpty">
              <strong>No registry profiles</strong>
              <span>Add one below, then Porto verifies it with a real image pull before use.</span>
            </div>
          )}
          {items.map((profile) => {
            const state = registryState(profile)
            return (
              <article className={`registryProfile ${state.className}`} key={profile.id}>
                <div className="registryProfileIdentity">
                  <strong>{profile.name}</strong>
                  <span>{profile.server}</span>
                </div>
                <div className="registryProfileSignal">
                  <strong>{state.label}</strong>
                  <span>{profile.verified ? verifiedTime(profile.lastVerifiedAt) : profile.lastError || 'Saved credential is inactive until verification succeeds.'}</span>
                </div>
                <div className="registryProfileActions">
                  <button type="button" onClick={() => { setEditingID(profile.id); setDraft(profileDraft(profile)) }}>Edit</button>
                  <button type="button" disabled={verifyingID !== null} onClick={() => verify(profile)}>
                    {verifyingID === profile.id ? 'Verifying…' : 'Verify pull'}
                  </button>
                  <button className="destructiveAction" type="button" onClick={() => remove(profile)}>Remove</button>
                </div>
              </article>
            )
          })}
        </div>

        <form className="registryForm" onSubmit={save}>
          <div className="registryFormHeader">
            <div>
              <strong>{editingID === null ? 'New registry profile' : 'Edit registry profile'}</strong>
              <span>{editingID === null ? 'Credential required' : 'Leave credential blank to keep the saved value'}</span>
            </div>
            {editingID !== null && <button type="button" onClick={reset}>Cancel edit</button>}
          </div>
          <div className="settingsFieldGrid">
            <label className="settingsField">
              <span>Provider</span>
              <select value={draft.provider} onChange={(event) => selectProvider(event.target.value as RegistryProvider)}>
                {Object.entries(PROVIDER_DEFAULTS).map(([value, details]) => (
                  <option value={value} key={value}>{details.label}</option>
                ))}
              </select>
            </label>
            <label className="settingsField">
              <span>Profile name</span>
              <input type="text" value={draft.name} maxLength={80} required onChange={(event) => setDraft({ ...draft, name: event.target.value })} />
            </label>
            <label className="settingsField">
              <span>Registry server</span>
              <input
                type="text"
                value={draft.server}
                placeholder="registry.example.com"
                readOnly={!customServer}
                required
                onChange={(event) => setDraft({ ...draft, server: event.target.value })}
              />
              <small>HTTPS registry host. Preset providers use their canonical endpoint.</small>
            </label>
            <label className="settingsField">
              <span>Username</span>
              <input type="text" value={draft.username} autoComplete="username" required onChange={(event) => setDraft({ ...draft, username: event.target.value })} />
            </label>
            <label className="settingsField">
              <span>Access token or password</span>
              <input
                type="password"
                value={draft.credential}
                autoComplete="off"
                required={editingID === null}
                placeholder={editingID === null ? 'Required' : 'Keep saved credential'}
                onChange={(event) => setDraft({ ...draft, credential: event.target.value })}
              />
            </label>
            <label className="settingsField">
              <span>Verification image</span>
              <input
                type="text"
                value={draft.testImage}
                placeholder={draft.provider === 'github' ? 'ghcr.io/owner/image:tag' : draft.provider === 'gitlab' ? 'registry.gitlab.com/group/image:tag' : 'registry.example.com/team/image:tag'}
                required
                onChange={(event) => setDraft({ ...draft, testImage: event.target.value })}
              />
              <small>Saving verifies the credential by pulling this image into Porto.</small>
            </label>
          </div>
          <label className="toggleRow registryEnable">
            <span><strong>Use after verification</strong><small>Disabled profiles remain saved but are never used for image pulls or clusters.</small></span>
            <input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} />
          </label>
          <button type="submit" disabled={saving || verifyingID !== null}>
            {saving ? 'Saving…' : editingID === null ? 'Save and verify' : 'Update and verify'}
          </button>
        </form>
      </div>
    </section>
  )
}
