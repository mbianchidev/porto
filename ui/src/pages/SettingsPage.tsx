import { useState } from 'react'
import { apiSend, errorMessage } from '../api'
import { useMessages } from '../useMessages'
import type { IntegrationStatus, KillSwitchCleanupResult, KillSwitchStatus, RuntimeFeatureName, RuntimeFeatures, Settings } from '../types'

const RUNTIME_LABELS: Record<RuntimeFeatureName, string> = {
  docker: 'Docker',
  kubernetes: 'Kubernetes',
  vms: 'Virtual machines',
}

export function SettingsPage({
  settings,
  onSettingsSaved,
  sqlNotSoLiteStatus,
  sendboxStatus,
  killSwitchStatus,
  onIntegrationsChanged,
}: {
  settings: Settings | null
  onSettingsSaved: (settings: Settings) => void
  sqlNotSoLiteStatus: IntegrationStatus | null
  sendboxStatus: IntegrationStatus | null
  killSwitchStatus: KillSwitchStatus | null
  onIntegrationsChanged: () => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [priorSettings, setPriorSettings] = useState(settings)
  const [draft, setDraft] = useState<Settings | null>(settings)
  const [protectedBranches, setProtectedBranches] = useState(settings?.protectedBranches.join(', ') ?? '')
  const [savedSendboxEnabled, setSavedSendboxEnabled] = useState(settings?.sendboxEnabled ?? false)
  const [savedKillSwitchEnabled, setSavedKillSwitchEnabled] = useState(settings?.killSwitchEnabled ?? false)
  const [savedSQLNotSoLiteEnabled, setSavedSQLNotSoLiteEnabled] = useState(settings?.sqlNotSoLiteEnabled ?? false)
  // Runtime feature gates are tracked separately from `draft` because they take
  // effect immediately through /api/runtime/features rather than the batched
  // "Save changes" flow, so flipping one must never clobber unsaved edits to
  // branch cleanup or the protected-branches text field.
  const [runtimeFeatures, setRuntimeFeatures] = useState<RuntimeFeatures>({
    docker: settings?.dockerEnabled ?? false,
    kubernetes: settings?.kubernetesEnabled ?? false,
    vms: settings?.vmsEnabled ?? false,
  })
  const [runtimeBusy, setRuntimeBusy] = useState<RuntimeFeatureName | null>(null)

  // Sync editable draft state whenever the loaded/saved settings change, following
  // React's "adjust state during render" pattern instead of an effect so the
  // draft never briefly shows stale values after a save or the initial load.
  if (settings !== priorSettings) {
    setPriorSettings(settings)
    setDraft(settings)
    setProtectedBranches(settings?.protectedBranches.join(', ') ?? '')
    setSavedSendboxEnabled(settings?.sendboxEnabled ?? false)
    setSavedKillSwitchEnabled(settings?.killSwitchEnabled ?? false)
    setSavedSQLNotSoLiteEnabled(settings?.sqlNotSoLiteEnabled ?? false)
    setRuntimeFeatures({
      docker: settings?.dockerEnabled ?? false,
      kubernetes: settings?.kubernetesEnabled ?? false,
      vms: settings?.vmsEnabled ?? false,
    })
  }

  function updateDraft(key: keyof Omit<Settings, 'protectedBranches'>, value: boolean) {
    setDraft((current) => (current ? { ...current, [key]: value } : current))
  }

  async function setRuntimeFeature(feature: RuntimeFeatureName, enabled: boolean) {
    setRuntimeBusy(feature)
    try {
      const features = await apiSend<RuntimeFeatures>(`/api/runtime/features/${feature}/${enabled ? 'enable' : 'disable'}`, 'POST')
      setRuntimeFeatures(features)
      const currentSettings = settings ?? draft
      if (currentSettings) {
        onSettingsSaved({
          ...currentSettings,
          dockerEnabled: features.docker,
          kubernetesEnabled: features.kubernetes,
          vmsEnabled: features.vms,
        })
      }
      notifyNotice('settings', `${RUNTIME_LABELS[feature]} ${enabled ? 'enabled' : 'disabled'}.`)
    } catch (err) {
      notifyError('settings', errorMessage(err, `Unable to ${enabled ? 'enable' : 'disable'} ${RUNTIME_LABELS[feature]}`))
    } finally {
      setRuntimeBusy(null)
    }
  }

  async function save() {
    if (!draft) return
    if (draft.cleanupRemoteMerged && !settings?.cleanupRemoteMerged) {
      const confirmed = window.confirm('Remote cleanup permanently deletes fully merged branches from the Git remote. Enable it?')
      if (!confirmed) {
        updateDraft('cleanupRemoteMerged', false)
        return
      }
    }
    const nextSettings: Settings = {
      ...draft,
      protectedBranches: protectedBranches.split(',').map((branch) => branch.trim()).filter(Boolean),
      // Runtime feature gates are managed immediately via /api/runtime/features;
      // always send their current value so this save can never regress a toggle
      // flipped after `draft` was last synced from `settings`.
      dockerEnabled: runtimeFeatures.docker,
      kubernetesEnabled: runtimeFeatures.kubernetes,
      vmsEnabled: runtimeFeatures.vms,
    }
    try {
      const saved = await apiSend<Settings>('/api/settings', 'PUT', nextSettings)
      onSettingsSaved(saved)
      const enabled = [
        saved.sqlNotSoLiteEnabled && !savedSQLNotSoLiteEnabled ? 'sql-not-so-lite' : '',
        saved.sendboxEnabled && !savedSendboxEnabled ? 'Sendbox' : '',
        saved.killSwitchEnabled && !savedKillSwitchEnabled ? 'KillSwitch' : '',
      ].filter(Boolean)
      notifyNotice('settings', enabled.length > 0 ? `Settings saved. Enabled ${enabled.join(' and ')}.` : 'Settings saved.')
      onIntegrationsChanged()
    } catch (err) {
      notifyError('settings', errorMessage(err, 'Unable to save settings'))
    }
  }

  async function runKillSwitchAction(actionName: 'install' | 'sync' | 'cleanup') {
    if (actionName === 'cleanup' && !window.confirm('Run KillSwitch cleanup now? It may terminate stale dev servers using KillSwitch settings.')) return
    try {
      if (actionName === 'cleanup') {
        const result = await apiSend<KillSwitchCleanupResult>('/api/integrations/kill-switch/cleanup', 'POST')
        notifyNotice(
          'settings',
          result.killedCount > 0
            ? `KillSwitch terminated ${result.killedCount} stale dev server(s).`
            : result.autoKillEnabled
              ? `KillSwitch found ${result.candidateCount} candidate(s); none met the cleanup threshold.`
              : `KillSwitch found ${result.candidateCount} candidate(s), but auto-kill is disabled.`,
        )
      } else {
        await apiSend(`/api/integrations/kill-switch/${actionName}`, 'POST')
        notifyNotice('settings', actionName === 'install' ? 'KillSwitch installation started.' : 'KillSwitch port sync started.')
      }
      onIntegrationsChanged()
    } catch (err) {
      notifyError('settings', errorMessage(err, `KillSwitch ${actionName} failed`))
      onIntegrationsChanged()
    }
  }

  const killSwitchBusy = ['checking', 'installing', 'syncing', 'cleaning'].includes(killSwitchStatus?.state ?? '')

  return (
    <>
      <header className="pageIntro">
        <div>
          <h1>System settings</h1>
          <p>Configure branch cleanup and optional integrations away from daily project controls.</p>
        </div>
        <a className="buttonLink" href="#/localhost-ing">Back to localhost-ing</a>
      </header>

      <section className="hygiene" aria-labelledby="branch-hygiene-title">
        <div className="hygieneIntro">
          <h2 id="branch-hygiene-title">Keep merged work out of the way.</h2>
          <p>Porto checks every 10 seconds and removes only branches whose full history is already in the default branch.</p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span><strong>Clean up local branches immediately after merge</strong><small>Keeps the current, default, unmerged, and protected branches.</small></span>
            <input type="checkbox" checked={draft?.cleanupLocalMerged ?? false} disabled={!draft} onChange={(event) => updateDraft('cleanupLocalMerged', event.target.checked)} />
          </label>
          <label className="toggleRow destructive">
            <span><strong>Clean up remote branches immediately after merge</strong><small>Permanently deletes matching branches from the primary remote.</small></span>
            <input type="checkbox" checked={draft?.cleanupRemoteMerged ?? false} disabled={!draft} onChange={(event) => updateDraft('cleanupRemoteMerged', event.target.checked)} />
          </label>
          <label className="toggleRow">
            <span><strong>Prune stale remote-tracking branches</strong><small>Runs a non-interactive fetch and prune before remote cleanup.</small></span>
            <input type="checkbox" checked={draft?.pruneRemoteTracking ?? false} disabled={!draft || !draft.cleanupRemoteMerged} onChange={(event) => updateDraft('pruneRemoteTracking', event.target.checked)} />
          </label>
          <label className="protectedField">
            <span>Protected branch patterns</span>
            <input type="text" value={protectedBranches} disabled={!draft} onChange={(event) => setProtectedBranches(event.target.value)} placeholder="main, develop, release/*" />
            <small>Comma-separated names or glob patterns. The default and current branches are always protected.</small>
          </label>
          <button type="button" onClick={save} disabled={!draft}>Save changes</button>
        </div>
      </section>

      <section className="integration runtimeAccess" aria-labelledby="runtime-access-title">
        <div className="hygieneIntro">
          <h2 id="runtime-access-title">Turn on optional runtimes.</h2>
          <p>
            Docker, Kubernetes, and VM management stay off by default so Porto's native project
            controls are never affected. Each toggle takes effect immediately.
          </p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span><strong>Enable Docker</strong><small>Containers, images, builds, volumes, and networks.</small></span>
            <input
              type="checkbox"
              checked={runtimeFeatures.docker}
              disabled={runtimeBusy !== null}
              onChange={(event) => setRuntimeFeature('docker', event.target.checked)}
            />
          </label>
          <label className="toggleRow">
            <span><strong>Enable Kubernetes</strong><small>Pods, services, configs, secrets, nodes, and Porto-provisioned clusters.</small></span>
            <input
              type="checkbox"
              checked={runtimeFeatures.kubernetes}
              disabled={runtimeBusy !== null}
              onChange={(event) => setRuntimeFeature('kubernetes', event.target.checked)}
            />
          </label>
          <label className="toggleRow">
            <span><strong>Enable virtual machines</strong><small>Lima-backed VM image catalog and instances.</small></span>
            <input
              type="checkbox"
              checked={runtimeFeatures.vms}
              disabled={runtimeBusy !== null}
              onChange={(event) => setRuntimeFeature('vms', event.target.checked)}
            />
          </label>
        </div>
      </section>

      <section className="integration" aria-labelledby="sqlite-integration-title">
        <div className="hygieneIntro">
          <h2 id="sqlite-integration-title">Discover project SQLite databases.</h2>
          <p>Porto installs and runs sql-not-so-lite only when an orchestrated project contains a valid SQLite database.</p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span><strong>Enable sql-not-so-lite</strong><small>Requires Go only when Porto needs to install the pinned sqnsl binary.</small></span>
            <input type="checkbox" checked={draft?.sqlNotSoLiteEnabled ?? false} disabled={!draft} onChange={(event) => updateDraft('sqlNotSoLiteEnabled', event.target.checked)} />
          </label>
          <div className={`integrationStatus ${sqlNotSoLiteStatus?.state ?? 'idle'}`}>
            <strong>{sqlNotSoLiteStatus?.state ?? 'loading'}</strong>
            <span>{sqlNotSoLiteStatus?.message ?? 'Loading integration status.'}</span>
          </div>
          <button type="button" onClick={save} disabled={!draft}>Save integration setting</button>
        </div>
      </section>

      <section className="integration sendboxIntegration" aria-labelledby="sendbox-integration-title">
        <div className="hygieneIntro">
          <h2 id="sendbox-integration-title">Run configured projects in Sendbox.</h2>
          <p>Porto starts Sendbox independently for projects with<code> .sendbox.yaml</code>. Normal project controls stay unchanged.</p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span><strong>Enable Sendbox</strong><small>Requires Sendbox, macOS 26, and Apple Silicon. Porto does not install it.</small></span>
            <input type="checkbox" checked={draft?.sendboxEnabled ?? false} disabled={!draft} onChange={(event) => updateDraft('sendboxEnabled', event.target.checked)} />
          </label>
          <div className={`integrationStatus ${sendboxStatus?.state ?? 'idle'}`}>
            <strong>{sendboxStatus?.state ?? 'loading'}</strong>
            <span>{sendboxStatus?.message ?? 'Loading Sendbox status.'}</span>
          </div>
          <button type="button" onClick={save} disabled={!draft}>Save integration setting</button>
        </div>
      </section>

      <section className="integration killSwitchIntegration" aria-labelledby="kill-switch-integration-title">
        <div className="hygieneIntro">
          <h2 id="kill-switch-integration-title">Hand active dev ports to KillSwitch.</h2>
          <p>Porto registers only ports for processes it is actively managing. KillSwitch keeps those ports separate from your own watch list.</p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span><strong>Enable KillSwitch</strong><small>macOS only. Installation always requires an explicit click.</small></span>
            <input
              type="checkbox"
              checked={draft?.killSwitchEnabled ?? false}
              disabled={!draft || killSwitchStatus?.supported === false}
              onChange={(event) => updateDraft('killSwitchEnabled', event.target.checked)}
            />
          </label>
          <div className={`integrationStatus ${killSwitchStatus?.state ?? 'idle'}`}>
            <strong>{killSwitchStatus?.state ?? 'loading'}</strong>
            <span>{killSwitchStatus?.message ?? 'Loading KillSwitch status.'}</span>
            <div className="killSwitchMeta">
              <span>{killSwitchStatus?.version ?? 'version unavailable'}</span>
              <span>{killSwitchStatus?.syncedPorts.length ?? 0} active Porto port(s)</span>
              <span>
                {killSwitchStatus?.autoKillEnabled == null
                  ? 'cleanup policy in KillSwitch'
                  : killSwitchStatus.autoKillEnabled ? 'auto-kill on' : 'auto-kill off'}
              </span>
            </div>
          </div>
          <div className="integrationActions">
            <button type="button" onClick={save} disabled={!draft || killSwitchBusy}>Save integration setting</button>
            <button type="button" onClick={() => runKillSwitchAction('install')} disabled={!killSwitchStatus?.supported || killSwitchBusy}>
              {killSwitchStatus?.installed ? 'Update KillSwitch' : 'Install KillSwitch'}
            </button>
            <button
              type="button"
              onClick={() => runKillSwitchAction('sync')}
              disabled={!draft?.killSwitchEnabled || !killSwitchStatus?.installed || killSwitchBusy}
            >
              Sync active ports
            </button>
            <button
              className="destructiveAction"
              type="button"
              onClick={() => runKillSwitchAction('cleanup')}
              disabled={!draft?.killSwitchEnabled || !killSwitchStatus?.installed || killSwitchBusy}
            >
              Run cleanup now
            </button>
          </div>
        </div>
      </section>
    </>
  )
}
