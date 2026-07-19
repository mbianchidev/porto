import { useEffect, useState } from 'react'
import './App.css'

type Project = {
  id: number
  name: string
  path: string
  strategy: string
  command: string
  port: number
  hostname: string
  pid: number
  status: string
  branch: string
  dirty: boolean
  sendboxConfigured: boolean
  sendboxStatus: string
  sendboxMessage: string
}

type Settings = {
  cleanupLocalMerged: boolean
  cleanupRemoteMerged: boolean
  pruneRemoteTracking: boolean
  protectedBranches: string[]
  sqlNotSoLiteEnabled: boolean
  killSwitchEnabled: boolean
  sendboxEnabled: boolean
}

type IntegrationStatus = {
  state: 'disabled' | 'idle' | 'running' | 'ready' | 'error'
  message: string
  updatedAt: string
}

type KillSwitchStatus = {
  state: 'disabled' | 'idle' | 'checking' | 'missing' | 'installing' | 'syncing' | 'cleaning' | 'ready' | 'error' | 'unsupported'
  message: string
  updatedAt: string
  supported: boolean
  installed: boolean
  binaryPath?: string
  version?: string
  autoKillEnabled: boolean | null
  userPorts: number[]
  syncedPorts: number[]
  effectivePorts: number[]
}

type KillSwitchCleanupResult = {
  autoKillEnabled: boolean
  candidateCount: number
  killedCount: number
  killedProcesses: Array<{ pid: number }>
}

type CleanupResult = {
  localDeleted: string[]
  remoteDeleted: string[]
  pruned: boolean
}

type LogStream = 'all' | 'stdout' | 'stderr'

type LogLine = {
  projectId: number
  stream: string
  line: string
  createdAt: string
}

async function action(name: string, verb: string): Promise<Response> {
  const response = await fetch(`/api/projects/${name}/${verb}`, { method: 'POST' })
  if (!response.ok) throw new Error(await response.text())
  return response
}

function App() {
  const [projects, setProjects] = useState<Project[]>([])
  const [settings, setSettings] = useState<Settings | null>(null)
  const [savedLocalCleanup, setSavedLocalCleanup] = useState(false)
  const [savedRemoteCleanup, setSavedRemoteCleanup] = useState(false)
  const [savedSQLNotSoLiteEnabled, setSavedSQLNotSoLiteEnabled] = useState(false)
  const [savedSendboxEnabled, setSavedSendboxEnabled] = useState(false)
  const [savedKillSwitchEnabled, setSavedKillSwitchEnabled] = useState(false)
  const [protectedBranches, setProtectedBranches] = useState('')
  const [sqlNotSoLiteStatus, setSQLNotSoLiteStatus] = useState<IntegrationStatus | null>(null)
  const [sendboxStatus, setSendboxStatus] = useState<IntegrationStatus | null>(null)
  const [killSwitchStatus, setKillSwitchStatus] = useState<KillSwitchStatus | null>(null)
  const [logProjectName, setLogProjectName] = useState('')
  const [logStream, setLogStream] = useState<LogStream>('all')
  const [logLines, setLogLines] = useState<LogLine[]>([])
  const [logRefresh, setLogRefresh] = useState(0)
  const [logsLoading, setLogsLoading] = useState(false)
  const [logError, setLogError] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  async function refreshProjects() {
    try {
      const response = await fetch('/api/projects')
      if (!response.ok) throw new Error(await response.text())
      setProjects(await response.json())
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to load projects')
    }
  }

  async function refreshIntegrations() {
    try {
      const [sqlNotSoLiteResponse, sendboxResponse, killSwitchResponse] = await Promise.all([
        fetch('/api/integrations/sql-not-so-lite'),
        fetch('/api/integrations/sendbox'),
        fetch('/api/integrations/kill-switch'),
      ])
      if (!sqlNotSoLiteResponse.ok) throw new Error(await sqlNotSoLiteResponse.text())
      if (!sendboxResponse.ok) throw new Error(await sendboxResponse.text())
      if (!killSwitchResponse.ok) throw new Error(await killSwitchResponse.text())
      setSQLNotSoLiteStatus(await sqlNotSoLiteResponse.json())
      setSendboxStatus(await sendboxResponse.json())
      setKillSwitchStatus(await killSwitchResponse.json())
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to load integration status')
    }
  }

  async function load() {
    try {
      const [
        projectsResponse,
        settingsResponse,
        sqlNotSoLiteResponse,
        sendboxResponse,
        killSwitchResponse,
      ] = await Promise.all([
        fetch('/api/projects'),
        fetch('/api/settings'),
        fetch('/api/integrations/sql-not-so-lite'),
        fetch('/api/integrations/sendbox'),
        fetch('/api/integrations/kill-switch'),
      ])
      if (!projectsResponse.ok) throw new Error(await projectsResponse.text())
      if (!settingsResponse.ok) throw new Error(await settingsResponse.text())
      if (!sqlNotSoLiteResponse.ok) throw new Error(await sqlNotSoLiteResponse.text())
      if (!sendboxResponse.ok) throw new Error(await sendboxResponse.text())
      if (!killSwitchResponse.ok) throw new Error(await killSwitchResponse.text())
      const nextSettings: Settings = await settingsResponse.json()
      setProjects(await projectsResponse.json())
      setSettings(nextSettings)
      setSQLNotSoLiteStatus(await sqlNotSoLiteResponse.json())
      setSendboxStatus(await sendboxResponse.json())
      setKillSwitchStatus(await killSwitchResponse.json())
      setSavedLocalCleanup(nextSettings.cleanupLocalMerged)
      setSavedRemoteCleanup(nextSettings.cleanupRemoteMerged)
      setSavedSQLNotSoLiteEnabled(nextSettings.sqlNotSoLiteEnabled)
      setSavedSendboxEnabled(nextSettings.sendboxEnabled)
      setSavedKillSwitchEnabled(nextSettings.killSwitchEnabled)
      setProtectedBranches(nextSettings.protectedBranches.join(', '))
      setError('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to load projects')
    }
  }

  async function run(name: string, verb: string) {
    try {
      await action(name, verb)
      await refreshProjects()
      setNotice('')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Action failed')
    }
  }

  async function runSendbox(name: string, verb: 'start' | 'stop') {
    try {
      await action(name, `sendbox/${verb}`)
      await refreshProjects()
      setError('')
      setNotice(verb === 'start' ? 'Sendbox session started.' : 'Stopping Sendbox session.')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Sendbox action failed')
    }
  }

  function updateSetting(key: keyof Omit<Settings, 'protectedBranches'>, value: boolean) {
    setSettings((current) => current ? { ...current, [key]: value } : current)
  }

  async function saveSettings() {
    if (!settings) return
    if (settings.cleanupRemoteMerged && !savedRemoteCleanup) {
      const confirmed = window.confirm(
        'Remote cleanup permanently deletes fully merged branches from the Git remote. Enable it?',
      )
      if (!confirmed) {
        updateSetting('cleanupRemoteMerged', false)
        return
      }
    }
    const nextSettings = {
      ...settings,
      protectedBranches: protectedBranches
        .split(',')
        .map((branch) => branch.trim())
        .filter(Boolean),
    }
    try {
      const response = await fetch('/api/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(nextSettings),
      })
      if (!response.ok) throw new Error(await response.text())
      const saved: Settings = await response.json()
      setSettings(saved)
      setSavedLocalCleanup(saved.cleanupLocalMerged)
      setSavedRemoteCleanup(saved.cleanupRemoteMerged)
      setSavedSQLNotSoLiteEnabled(saved.sqlNotSoLiteEnabled)
      setSavedSendboxEnabled(saved.sendboxEnabled)
      setSavedKillSwitchEnabled(saved.killSwitchEnabled)
      setProtectedBranches(saved.protectedBranches.join(', '))
      setError('')
      const enabled = [
        saved.sqlNotSoLiteEnabled && !savedSQLNotSoLiteEnabled ? 'sql-not-so-lite' : '',
        saved.sendboxEnabled && !savedSendboxEnabled ? 'Sendbox' : '',
        saved.killSwitchEnabled && !savedKillSwitchEnabled ? 'KillSwitch' : '',
      ].filter(Boolean)
      setNotice(enabled.length > 0 ? `Settings saved. Enabled ${enabled.join(' and ')}.` : 'Settings saved.')
      await refreshIntegrations()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to save settings')
    }
  }

  async function cleanup(name: string) {
    try {
      const response = await action(name, 'cleanup-branches')
      const result: CleanupResult = await response.json()
      const deleted = [
        ...result.localDeleted.map((branch) => `local ${branch}`),
        ...result.remoteDeleted.map((branch) => `remote ${branch}`),
      ]
      setError('')
      setNotice(
        deleted.length > 0
          ? `Deleted ${deleted.join(', ')}.`
          : 'No fully merged, unprotected branches found.',
      )
      await refreshProjects()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Branch cleanup failed')
    }
  }

  async function runKillSwitchAction(actionName: 'install' | 'sync' | 'cleanup') {
    if (
      actionName === 'cleanup'
      && !window.confirm('Run KillSwitch cleanup now? It may terminate stale dev servers using KillSwitch settings.')
    ) {
      return
    }
    if (actionName === 'cleanup') {
      setKillSwitchStatus((current) => current ? {
        ...current,
        state: 'cleaning',
        message: 'Running KillSwitch dev cleanup.',
      } : current)
    }
    try {
      const response = await fetch(`/api/integrations/kill-switch/${actionName}`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      if (actionName === 'cleanup') {
        const result: KillSwitchCleanupResult = await response.json()
        setNotice(
          result.killedCount > 0
            ? `KillSwitch terminated ${result.killedCount} stale dev server(s).`
            : result.autoKillEnabled
              ? `KillSwitch found ${result.candidateCount} candidate(s); none met the cleanup threshold.`
              : `KillSwitch found ${result.candidateCount} candidate(s), but auto-kill is disabled.`,
        )
      } else {
        setKillSwitchStatus(await response.json())
        setNotice(actionName === 'install' ? 'KillSwitch installation started.' : 'KillSwitch port sync started.')
      }
      setError('')
      await refreshIntegrations()
      if (actionName === 'cleanup') {
        await refreshProjects()
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : `KillSwitch ${actionName} failed`)
      await refreshIntegrations()
    }
  }

  async function clearLogs() {
    if (!logProjectName) return
    const label = logStream === 'all' ? 'all logs' : `${logStream} logs`
    if (!window.confirm(`Clear ${label} for ${logProjectName}?`)) return
    try {
      const response = await fetch(
        `/api/projects/${encodeURIComponent(logProjectName)}/logs/clear?stream=${logStream}`,
        { method: 'POST' },
      )
      if (!response.ok) throw new Error(await response.text())
      const result: { deleted: number } = await response.json()
      setNotice(`Cleared ${result.deleted} ${logStream === 'all' ? '' : `${logStream} `}log line(s).`)
      setError('')
      setLogRefresh((value) => value + 1)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Unable to clear logs')
    }
  }

  useEffect(() => {
    load()
    const timer = window.setInterval(() => {
      refreshProjects()
      refreshIntegrations()
    }, 5000)
    return () => window.clearInterval(timer)
  }, [])

  useEffect(() => {
    if (!logProjectName) return
    let active = true
    const loadLogs = async (showLoading: boolean) => {
      if (showLoading) {
        setLogsLoading(true)
        setLogLines([])
      }
      try {
        const response = await fetch(
          `/api/projects/${encodeURIComponent(logProjectName)}/logs?limit=500&stream=${logStream}`,
        )
        if (!response.ok) throw new Error(await response.text())
        const lines: LogLine[] = await response.json()
        if (active) {
          setLogLines(lines)
          setLogError('')
        }
      } catch (err) {
        if (active) {
          setLogError(err instanceof Error ? err.message : 'Unable to load logs')
        }
      } finally {
        if (active && showLoading) setLogsLoading(false)
      }
    }
    loadLogs(true)
    const timer = window.setInterval(() => loadLogs(false), 2000)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [logProjectName, logStream, logRefresh])

  const killSwitchBusy = ['checking', 'installing', 'syncing', 'cleaning'].includes(killSwitchStatus?.state ?? '')
  const logProject = projects.find((project) => project.name === logProjectName)

  return (
    <main>
      <header className="hero">
        <div>
          <p className="eyebrow">Porto</p>
          <h1>Local Project Orchestrator</h1>
          <p>
            Discover runnable repos, start or stop them from one dashboard, and
            keep PID, port, logs, Git branch, and local hostnames in one small
            SQLite-backed daemon.
          </p>
        </div>
        <button type="button" onClick={refreshProjects}>Refresh</button>
      </header>

      {error && <div className="error">{error}</div>}
      {notice && <div className="notice">{notice}</div>}

      <section className="hygiene" aria-labelledby="branch-hygiene-title">
        <div className="hygieneIntro">
          <p className="eyebrow">Branch hygiene</p>
          <h2 id="branch-hygiene-title">Keep merged work out of the way.</h2>
          <p>
            Porto checks every 10 seconds and removes only branches whose full
            history is already in the default branch.
          </p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span>
              <strong>Clean up local branches immediately after merge</strong>
              <small>Keeps the current, default, unmerged, and protected branches.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.cleanupLocalMerged ?? false}
              disabled={!settings}
              onChange={(event) => updateSetting('cleanupLocalMerged', event.target.checked)}
            />
          </label>
          <label className="toggleRow destructive">
            <span>
              <strong>Clean up remote branches immediately after merge</strong>
              <small>Permanently deletes matching branches from the primary remote.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.cleanupRemoteMerged ?? false}
              disabled={!settings}
              onChange={(event) => updateSetting('cleanupRemoteMerged', event.target.checked)}
            />
          </label>
          <label className="toggleRow">
            <span>
              <strong>Prune stale remote-tracking branches</strong>
              <small>Runs a non-interactive fetch and prune before remote cleanup.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.pruneRemoteTracking ?? false}
              disabled={!settings || !settings.cleanupRemoteMerged}
              onChange={(event) => updateSetting('pruneRemoteTracking', event.target.checked)}
            />
          </label>
          <label className="protectedField">
            <span>Protected branch patterns</span>
            <input
              type="text"
              value={protectedBranches}
              disabled={!settings}
              onChange={(event) => setProtectedBranches(event.target.value)}
              placeholder="main, develop, release/*"
            />
            <small>Comma-separated names or glob patterns. The default and current branches are always protected.</small>
          </label>
          <button type="button" onClick={saveSettings} disabled={!settings}>Save changes</button>
        </div>
      </section>

      <section className="integration" aria-labelledby="sqlite-integration-title">
        <div className="hygieneIntro">
          <p className="eyebrow">Optional integration</p>
          <h2 id="sqlite-integration-title">Discover project SQLite databases.</h2>
          <p>
            Porto installs and runs sql-not-so-lite only when an orchestrated
            project contains a valid SQLite database.
          </p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span>
              <strong>Enable sql-not-so-lite</strong>
              <small>Requires Go only when Porto needs to install the pinned sqnsl binary.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.sqlNotSoLiteEnabled ?? false}
              disabled={!settings}
              onChange={(event) => updateSetting('sqlNotSoLiteEnabled', event.target.checked)}
            />
          </label>
          <div className={`integrationStatus ${sqlNotSoLiteStatus?.state ?? 'idle'}`}>
            <strong>{sqlNotSoLiteStatus?.state ?? 'loading'}</strong>
            <span>{sqlNotSoLiteStatus?.message ?? 'Loading integration status.'}</span>
          </div>
          <button type="button" onClick={saveSettings} disabled={!settings}>Save integration setting</button>
        </div>
      </section>

      <section className="integration sendboxIntegration" aria-labelledby="sendbox-integration-title">
        <div className="hygieneIntro">
          <p className="eyebrow">Optional integration</p>
          <h2 id="sendbox-integration-title">Run configured projects in Sendbox.</h2>
          <p>
            Porto starts Sendbox independently for projects with
            <code> .sendbox.yaml</code>. Normal project controls stay unchanged.
          </p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span>
              <strong>Enable Sendbox</strong>
              <small>Requires Sendbox, macOS 26, and Apple Silicon. Porto does not install it.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.sendboxEnabled ?? false}
              disabled={!settings}
              onChange={(event) => updateSetting('sendboxEnabled', event.target.checked)}
            />
          </label>
          <div className={`integrationStatus ${sendboxStatus?.state ?? 'idle'}`}>
            <strong>{sendboxStatus?.state ?? 'loading'}</strong>
            <span>{sendboxStatus?.message ?? 'Loading Sendbox status.'}</span>
          </div>
          <button type="button" onClick={saveSettings} disabled={!settings}>Save integration setting</button>
        </div>
      </section>

      <section className="integration killSwitchIntegration" aria-labelledby="kill-switch-integration-title">
        <div className="hygieneIntro">
          <p className="eyebrow">Optional integration</p>
          <h2 id="kill-switch-integration-title">Hand active dev ports to KillSwitch.</h2>
          <p>
            Porto registers only ports for processes it is actively managing.
            KillSwitch keeps those ports separate from your own watch list.
          </p>
        </div>
        <div className="hygieneControls">
          <label className="toggleRow">
            <span>
              <strong>Enable KillSwitch</strong>
              <small>macOS only. Installation always requires an explicit click.</small>
            </span>
            <input
              type="checkbox"
              checked={settings?.killSwitchEnabled ?? false}
              disabled={!settings || killSwitchStatus?.supported === false}
              onChange={(event) => updateSetting('killSwitchEnabled', event.target.checked)}
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
            <button type="button" onClick={saveSettings} disabled={!settings || killSwitchBusy}>
              Save integration setting
            </button>
            <button
              type="button"
              onClick={() => runKillSwitchAction('install')}
              disabled={!killSwitchStatus?.supported || killSwitchBusy}
            >
              {killSwitchStatus?.installed ? 'Update KillSwitch' : 'Install KillSwitch'}
            </button>
            <button
              type="button"
              onClick={() => runKillSwitchAction('sync')}
              disabled={!settings?.killSwitchEnabled || !killSwitchStatus?.installed || killSwitchBusy}
            >
              Sync active ports
            </button>
            <button
              className="destructiveAction"
              type="button"
              onClick={() => runKillSwitchAction('cleanup')}
              disabled={!settings?.killSwitchEnabled || !killSwitchStatus?.installed || killSwitchBusy}
            >
              Run cleanup now
            </button>
          </div>
        </div>
      </section>

      {logProjectName && (
        <section className="logConsole" aria-labelledby="process-console-title">
          <div className="consoleHeader">
            <div>
              <p className="eyebrow">Process console</p>
              <h2 id="process-console-title">{logProjectName}</h2>
              <p>
                {logProject?.status ?? 'unknown'} · {logProject?.pid ? `PID ${logProject.pid}` : 'no active PID'}
              </p>
            </div>
            <div className="consoleActions">
              <button type="button" onClick={() => setLogRefresh((value) => value + 1)}>Refresh</button>
              <button className="destructiveAction" type="button" onClick={clearLogs}>Clear visible</button>
              <button type="button" onClick={() => setLogProjectName('')}>Close</button>
            </div>
          </div>
          <div className="streamTabs" role="tablist" aria-label="Log stream">
            {(['all', 'stdout', 'stderr'] as const).map((stream) => (
              <button
                type="button"
                role="tab"
                aria-selected={logStream === stream}
                className={logStream === stream ? 'active' : ''}
                key={stream}
                onClick={() => {
                  setLogLines([])
                  setLogStream(stream)
                }}
              >
                {stream}
              </button>
            ))}
          </div>
          <div className="logViewport" role="log" aria-live="polite" aria-busy={logsLoading}>
            {logError && <div className="logEmpty errorLine">{logError}</div>}
            {!logError && logsLoading && logLines.length === 0 && (
              <div className="logEmpty">Loading process output…</div>
            )}
            {!logError && !logsLoading && logLines.length === 0 && (
              <div className="logEmpty">No {logStream === 'all' ? '' : `${logStream} `}output captured yet.</div>
            )}
            {!logError && logLines.map((line, index) => (
              <div className={`logLine ${line.stream}`} key={`${line.createdAt}-${index}`}>
                <time dateTime={line.createdAt}>
                  {new Date(line.createdAt).toLocaleTimeString([], { hour12: false })}
                </time>
                <span className="streamLabel">{line.stream}</span>
                <span className="logMessage">{line.line}</span>
              </div>
            ))}
          </div>
        </section>
      )}

      <section className="grid">
        {projects.length === 0 && (
          <article className="empty">
            <h2>No projects yet</h2>
            <p>Run <code>porto scan ~/code --depth 3</code> to discover projects.</p>
          </article>
        )}
        {projects.map((project) => (
          <article className="card" key={project.id}>
            <div className="cardHeader">
              <div>
                <h2>{project.name}</h2>
                <p>{project.path}</p>
              </div>
              <span className={`status ${project.status}`}>{project.status}</span>
            </div>

            <dl>
              <div><dt>Port</dt><dd>{project.port || 'unassigned'}</dd></div>
              <div><dt>PID</dt><dd>{project.pid || '—'}</dd></div>
              <div><dt>Branch</dt><dd>{project.branch}{project.dirty ? ' *' : ''}</dd></div>
              <div><dt>Strategy</dt><dd>{project.strategy}</dd></div>
              <div>
                <dt>HTTPS host</dt>
                <dd>
                  <a href={`https://${project.hostname}.porto.local:37681`} target="_blank" rel="noreferrer">
                    {project.hostname}.porto.local:37681
                  </a>
                </dd>
              </div>
              <div>
                <dt>Sendbox</dt>
                <dd
                  className={`sendboxState ${project.sendboxStatus}`}
                  title={project.sendboxMessage}
                >
                  {project.sendboxConfigured ? project.sendboxStatus : 'not configured'}
                </dd>
              </div>
            </dl>

            <code className="command">{project.command}</code>

            <div className="actions">
              <button type="button" onClick={() => run(project.name, 'start')}>Start</button>
              <button type="button" onClick={() => run(project.name, 'stop')}>Stop</button>
              <button type="button" onClick={() => run(project.name, 'restart')}>Restart</button>
              <button type="button" onClick={() => run(project.name, 'kill')}>Kill</button>
              <button
                className="logsButton"
                type="button"
                onClick={() => {
                  setLogLines([])
                  setLogProjectName(project.name)
                  setLogStream('all')
                }}
              >
                View logs
              </button>
              {project.sendboxConfigured && (
                <button
                  className="sendboxButton"
                  type="button"
                  disabled={
                    !savedSendboxEnabled
                    || sendboxStatus?.state !== 'ready'
                    || project.sendboxStatus === 'running'
                    || project.sendboxStatus === 'stopping'
                  }
                  onClick={() => runSendbox(project.name, 'start')}
                >
                  Run in Sendbox
                </button>
              )}
              {(project.sendboxConfigured
                || project.sendboxStatus === 'running'
                || project.sendboxStatus === 'stopping') && (
                <button
                  className="sendboxButton"
                  type="button"
                  disabled={project.sendboxStatus !== 'running'}
                  onClick={() => runSendbox(project.name, 'stop')}
                >
                  Stop Sendbox
                </button>
              )}
              <button
                className="cleanupButton"
                type="button"
                disabled={!savedLocalCleanup && !savedRemoteCleanup}
                onClick={() => cleanup(project.name)}
              >
                Clean merged branches
              </button>
            </div>
          </article>
        ))}
      </section>
    </main>
  )
}

export default App
