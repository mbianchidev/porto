import { useEffect, useId, useRef, useState } from 'react'
import './App.css'

type Project = {
  id: number
  name: string
  path: string
  strategy: string
  command: string
  port: number
  hostname: string
  baseHostname: string
  httpsUrl: string
  sourcePath: string
  managedInstance: boolean
  defaultBranch: string
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
type Page = 'projects' | 'settings'
type ProjectView = 'list' | 'tiles'
type ProjectStatusFilter = 'all' | 'starting' | 'running' | 'stopped' | 'error'
type ProjectActionIcon = 'play' | 'stop' | 'restart' | 'kill' | 'setup' | 'logs' | 'sendboxPlay' | 'sendboxStop' | 'cleanup' | 'remove'

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

function ProjectActionButton({
  label,
  icon,
  className = '',
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & {
  label: string
  icon: ProjectActionIcon
}) {
  return (
    <button
      {...props}
      className={`iconButton ${className}`.trim()}
      type="button"
      aria-label={label}
      data-tooltip={label}
    >
      <svg viewBox="0 0 24 24" aria-hidden="true">
        {icon === 'play' && <path d="m8 5 11 7-11 7Z" />}
        {icon === 'stop' && <rect x="6" y="6" width="12" height="12" rx="2" />}
        {icon === 'restart' && (
          <>
            <path d="M20 11a8 8 0 1 0-2.34 5.66" />
            <path d="M20 4v7h-7" />
          </>
        )}
        {icon === 'kill' && (
          <>
            <path d="M8.5 3h7l4.5 5v8l-4.5 5h-7L4 16V8Z" />
            <path d="m9 9 6 6m0-6-6 6" />
          </>
        )}
        {icon === 'setup' && (
          <>
            <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" />
            <path d="m4.3 7.7 7.7 4.2 7.7-4.2M12 12v9" />
          </>
        )}
        {icon === 'logs' && (
          <>
            <path d="M5 5h14v14H5Z" />
            <path d="m8 9 2 2-2 2m4 1h4" />
          </>
        )}
        {icon === 'sendboxPlay' && (
          <>
            <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" />
            <path d="m4.3 7.7 7.7 4.2 7.7-4.2M12 12v9" />
            <path d="m10 8 4 2.2-4 2.3Z" />
          </>
        )}
        {icon === 'sendboxStop' && (
          <>
            <path d="m12 3 8 4.5v9L12 21l-8-4.5v-9Z" />
            <rect x="9.5" y="8.5" width="5" height="5" rx="0.5" />
          </>
        )}
        {icon === 'cleanup' && (
          <>
            <path d="M7 4v5a3 3 0 0 0 3 3h7" />
            <path d="m14 9 3 3-3 3" />
            <path d="M7 20v-3a3 3 0 0 1 3-3" />
          </>
        )}
        {icon === 'remove' && (
          <>
            <path d="M5 7h14M9 7V4h6v3m-8 0 1 13h8l1-13" />
            <path d="M10 11v5m4-5v5" />
          </>
        )}
      </svg>
      <span className="visuallyHidden">{label}</span>
    </button>
  )
}

function BranchPicker({
  label,
  value,
  options,
  defaultBranch,
  placeholder,
  disabled,
  onOpen,
  onSelect,
}: {
  label: string
  value: string
  options: string[]
  defaultBranch: string
  placeholder: string
  disabled: boolean
  onOpen: () => void
  onSelect: (branch: string) => void
}) {
  const id = useId()
  const inputID = `${id}-input`
  const listID = `${id}-list`
  const rootRef = useRef<HTMLDivElement>(null)
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const pinned = [defaultBranch, 'main', 'master'].filter(Boolean)
  const orderedOptions = [...new Set(options)]
    .sort((left, right) => {
      const leftPinned = pinned.indexOf(left)
      const rightPinned = pinned.indexOf(right)
      if (leftPinned !== -1 || rightPinned !== -1) {
        if (leftPinned === -1) return 1
        if (rightPinned === -1) return -1
        return leftPinned - rightPinned
      }
      return left.localeCompare(right)
    })
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filteredOptions = orderedOptions.filter((branch) => (
    normalizedQuery === '' || branch.toLocaleLowerCase().includes(normalizedQuery)
  ))

  useEffect(() => {
    if (!open) return
    const close = (event: PointerEvent) => {
      if (!rootRef.current?.contains(event.target as Node)) {
        setOpen(false)
        setQuery('')
      }
    }
    window.addEventListener('pointerdown', close)
    return () => window.removeEventListener('pointerdown', close)
  }, [open])

  function showOptions() {
    if (disabled) return
    onOpen()
    setQuery('')
    setOpen(true)
  }

  return (
    <div className="branchField">
      <label htmlFor={inputID}>{label}</label>
      <div className={`branchPicker ${open ? 'open' : ''}`} ref={rootRef}>
        <svg viewBox="0 0 24 24" aria-hidden="true">
          <circle cx="11" cy="11" r="6.5" />
          <path d="m16 16 4 4" />
        </svg>
        <input
          id={inputID}
          type="search"
          role="combobox"
          aria-autocomplete="list"
          aria-controls={listID}
          aria-expanded={open}
          value={open ? query : value}
          placeholder={placeholder}
          disabled={disabled}
          onFocus={showOptions}
          onClick={showOptions}
          onChange={(event) => {
            setQuery(event.target.value)
            setOpen(true)
          }}
          onKeyDown={(event) => {
            if (event.key === 'Escape') {
              setOpen(false)
              setQuery('')
              event.currentTarget.blur()
            }
            if (event.key === 'Enter' && filteredOptions.length === 1) {
              event.preventDefault()
              onSelect(filteredOptions[0])
              setOpen(false)
              setQuery('')
            }
          }}
        />
        <span className="branchChevron" aria-hidden="true">⌄</span>
        {open && (
          <div className="branchMenu" id={listID} role="listbox">
            {filteredOptions.length === 0 && (
              <span className="branchEmpty">No matching branches</span>
            )}
            {filteredOptions.map((branch) => (
              <button
                type="button"
                role="option"
                aria-selected={branch === value}
                className={branch === value ? 'selected' : ''}
                key={branch}
                onMouseDown={(event) => event.preventDefault()}
                onClick={() => {
                  onSelect(branch)
                  setOpen(false)
                  setQuery('')
                }}
              >
                <span>{branch}</span>
                {branch === defaultBranch && <small>default</small>}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

function App() {
  const [page, setPage] = useState<Page>(() => window.location.hash === '#/settings' ? 'settings' : 'projects')
  const [projectView, setProjectView] = useState<ProjectView>(() => {
    const savedView = window.localStorage.getItem('porto-project-view')
    return savedView === 'tiles' ? 'tiles' : 'list'
  })
  const [projects, setProjects] = useState<Project[]>([])
  const [projectQuery, setProjectQuery] = useState('')
  const [projectStatusFilter, setProjectStatusFilter] = useState<ProjectStatusFilter>('all')
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
  const [logProjectID, setLogProjectID] = useState<number | null>(null)
  const [logStream, setLogStream] = useState<LogStream>('all')
  const [logLines, setLogLines] = useState<LogLine[]>([])
  const [logRefresh, setLogRefresh] = useState(0)
  const [logFocusRequest, setLogFocusRequest] = useState(0)
  const [setupProjectID, setSetupProjectID] = useState<number | null>(null)
  const [branchOptions, setBranchOptions] = useState<Record<number, string[]>>({})
  const [branchBusyID, setBranchBusyID] = useState<number | null>(null)
  const [logsLoading, setLogsLoading] = useState(false)
  const [logError, setLogError] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const logConsoleRef = useRef<HTMLElement>(null)
  const normalizedProjectQuery = projectQuery.trim().toLocaleLowerCase()
  const sourceKey = (project: Project) => project.sourcePath || project.path
  const matchingSources = new Set(
    projects
      .filter((project) => {
        if (normalizedProjectQuery === '') return true
        return [project.name, project.branch, project.hostname, sourceKey(project)]
          .some((value) => value.toLocaleLowerCase().includes(normalizedProjectQuery))
      })
      .map(sourceKey),
  )
  const filteredProjects = projects.filter((project) => {
    const matchesName = matchingSources.has(sourceKey(project))
    const matchesStatus = projectStatusFilter === 'all'
      || project.status === projectStatusFilter
      || (
        projectStatusFilter === 'error'
        && project.status !== 'starting'
        && project.status !== 'running'
        && project.status !== 'stopped'
      )
    return matchesName && matchesStatus
  })
  const projectGroupTotals = projects.reduce<Record<string, number>>((totals, project) => {
    const key = sourceKey(project)
    totals[key] = (totals[key] ?? 0) + 1
    return totals
  }, {})
  const projectGroups = Object.entries(
    filteredProjects.reduce<Record<string, Project[]>>((groups, project) => {
      const key = sourceKey(project)
      groups[key] = [...(groups[key] ?? []), project]
      return groups
    }, {}),
  )
    .map(([key, groupProjects]) => ({
      key,
      projects: groupProjects.sort((left, right) => {
        if (left.managedInstance !== right.managedInstance) return left.managedInstance ? 1 : -1
        const leftDefault = left.branch === left.defaultBranch
        const rightDefault = right.branch === right.defaultBranch
        if (leftDefault !== rightDefault) return leftDefault ? -1 : 1
        return left.branch.localeCompare(right.branch)
      }),
      total: projectGroupTotals[key] ?? groupProjects.length,
    }))
    .sort((left, right) => left.projects[0].name.localeCompare(right.projects[0].name))
  const sourceProjectCount = new Set(projects.map(sourceKey)).size
  const managedInstanceCount = projects.filter((project) => project.managedInstance).length
  const projectStatusCounts = projects.reduce(
    (counts, project) => {
      if (project.status === 'starting' || project.status === 'running' || project.status === 'stopped') {
        counts[project.status] += 1
      } else {
        counts.error += 1
      }
      return counts
    },
    { starting: 0, running: 0, stopped: 0, error: 0 },
  )

  useEffect(() => {
    if (!error) return
    const timer = window.setTimeout(() => setError(''), 7000)
    return () => window.clearTimeout(timer)
  }, [error])

  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 4500)
    return () => window.clearTimeout(timer)
  }, [notice])

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

  async function loadBranches(project: Project) {
      if (branchOptions[project.id]) return
      try {
        const response = await fetch(`/api/projects/${project.id}/branches`)
        if (!response.ok) throw new Error(await response.text())
        const result: { branches: string[] } = await response.json()
        setBranchOptions((current) => ({ ...current, [project.id]: result.branches }))
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Unable to load branches')
      }
    }

  async function switchBranch(project: Project, branch: string) {
      if (!branch || branch === project.branch) return
      setBranchBusyID(project.id)
      try {
        const response = await fetch(`/api/projects/${project.id}/branch`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ branch }),
        })
        if (!response.ok) throw new Error(await response.text())
        setError('')
        setNotice(`Switched ${project.name} to ${branch}${project.status === 'running' || project.status === 'starting' ? ' and restarted it' : ''}.`)
        await refreshProjects()
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Branch switch failed')
      } finally {
        setBranchBusyID(null)
      }
    }

  async function createInstance(project: Project, branch: string) {
      if (!branch) return
      setBranchBusyID(project.id)
      try {
        const response = await fetch(`/api/projects/${project.id}/instances`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ branch }),
        })
        if (!response.ok) throw new Error(await response.text())
        setError('')
        setNotice(`Created and prepared a ${project.name} instance for ${branch}.`)
        setBranchOptions({})
        await refreshProjects()
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Unable to create branch instance')
      } finally {
        setBranchBusyID(null)
      }
    }

  async function removeInstance(project: Project) {
      const dirtyWarning = project.dirty ? ' Uncommitted changes in this instance will be discarded.' : ''
      if (!window.confirm(`Delete the ${project.branch} instance of ${project.name}?${dirtyWarning}`)) return
      setBranchBusyID(project.id)
      try {
        const response = await fetch(`/api/projects/${project.id}/instance`, { method: 'DELETE' })
        if (!response.ok) throw new Error(await response.text())
        if (logProjectID === project.id) setLogProjectID(null)
        setError('')
        setNotice(`Deleted the ${project.branch} instance of ${project.name}.`)
        setBranchOptions({})
        await refreshProjects()
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Unable to remove branch instance')
      } finally {
        setBranchBusyID(null)
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
    if (logProjectID == null) return
    const project = projects.find((item) => item.id === logProjectID)
    if (!project) return
    const label = logStream === 'all' ? 'all logs' : `${logStream} logs`
    if (!window.confirm(`Clear ${label} for ${project.name}?`)) return
    try {
      const response = await fetch(
        `/api/projects/${logProjectID}/logs/clear?stream=${logStream}`,
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

  function viewLogs(id: number) {
    setLogLines([])
    setLogProjectID(id)
    setLogStream('all')
    setLogFocusRequest((value) => value + 1)
  }

  async function setupDependencies(project: Project) {
    viewLogs(project.id)
    setSetupProjectID(project.id)
    setError('')
    setNotice('')
    try {
      const response = await fetch(`/api/projects/${project.id}/setup`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      const result: { commands: string[] } = await response.json()
      setNotice(`Dependency setup completed with ${result.commands.join(' then ')}.`)
      setLogRefresh((value) => value + 1)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Dependency setup failed')
      setLogRefresh((value) => value + 1)
    } finally {
      setSetupProjectID(null)
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
    const handleHashChange = () => {
      setPage(window.location.hash === '#/settings' ? 'settings' : 'projects')
    }
    window.addEventListener('hashchange', handleHashChange)
    return () => window.removeEventListener('hashchange', handleHashChange)
  }, [])

  useEffect(() => {
    if (logProjectID == null) return
    let active = true
    const loadLogs = async (showLoading: boolean) => {
      if (showLoading) {
        setLogsLoading(true)
        setLogLines([])
      }
      try {
        const response = await fetch(
          `/api/projects/${logProjectID}/logs?limit=500&stream=${logStream}`,
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
  }, [logProjectID, logStream, logRefresh])

  useEffect(() => {
    if (logProjectID == null) return
    const frame = window.requestAnimationFrame(() => {
      logConsoleRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    })
    return () => window.cancelAnimationFrame(frame)
  }, [logProjectID, logFocusRequest])

  const killSwitchBusy = ['checking', 'installing', 'syncing', 'cleaning'].includes(killSwitchStatus?.state ?? '')
  const logProject = projects.find((project) => project.id === logProjectID)

  return (
    <main>
      <header className="appHeader">
        <a className="brand" href="#/" aria-label="Porto projects">
          <span className="brandMark" aria-hidden="true">P</span>
          <span>
            <strong>Porto</strong>
            <small>Local orchestrator</small>
          </span>
        </a>
        <nav className="primaryNav" aria-label="Primary navigation">
          <a className={page === 'projects' ? 'active' : ''} href="#/">Projects</a>
          <a className={page === 'settings' ? 'active' : ''} href="#/settings">Settings</a>
        </nav>
      </header>

      {error && <div className="errorBanner banner" role="alert">{error}</div>}
      {notice && <div className="notice banner" role="status">{notice}</div>}

      {page === 'settings' && (
        <>
          <header className="pageIntro">
            <div>
              <p className="eyebrow">Settings</p>
              <h1>Keep the dashboard focused.</h1>
              <p>Configure branch cleanup and optional integrations away from daily project controls.</p>
            </div>
            <a className="buttonLink" href="#/">Back to projects</a>
          </header>

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
        </>
      )}

      {page === 'projects' && logProjectID != null && (
        <section ref={logConsoleRef} className="logConsole" aria-labelledby="process-console-title">
          <div className="consoleHeader">
            <div>
              <p className="eyebrow">Process console</p>
              <h2 id="process-console-title">
                {logProject?.name ?? 'Project'}{logProject?.branch ? ` · ${logProject.branch}` : ''}
              </h2>
              <p>
                {logProject?.status ?? 'unknown'} · {logProject?.pid ? `PID ${logProject.pid}` : 'no active PID'}
              </p>
            </div>
            <div className="consoleActions">
              <button type="button" onClick={() => setLogRefresh((value) => value + 1)}>Refresh</button>
              <button className="destructiveAction" type="button" onClick={clearLogs}>Clear visible</button>
              <button type="button" onClick={() => setLogProjectID(null)}>Close</button>
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

      {page === 'projects' && (
        <>
          <header className="projectsHeader">
            <div>
              <p className="eyebrow">Projects</p>
              <h1>Local work, in motion.</h1>
              <p>
                {sourceProjectCount} source {sourceProjectCount === 1 ? 'project' : 'projects'}
                {managedInstanceCount > 0 && ` · ${managedInstanceCount} branch ${managedInstanceCount === 1 ? 'instance' : 'instances'}`}
              </p>
            </div>
            <div className="projectTools">
              <div className="viewSwitch" role="group" aria-label="Project view">
                {(['list', 'tiles'] as const).map((view) => (
                  <button
                    type="button"
                    className={projectView === view ? 'active' : ''}
                    aria-pressed={projectView === view}
                    key={view}
                    onClick={() => {
                      setProjectView(view)
                      window.localStorage.setItem('porto-project-view', view)
                    }}
                  >
                    {view === 'list' ? 'List' : 'Tiles'}
                  </button>
                ))}
              </div>
              <button type="button" onClick={refreshProjects}>Refresh</button>
            </div>
          </header>

          <div className="projectFilters">
            <label className="projectSearch">
              <span className="visuallyHidden">Filter projects by name</span>
              <svg viewBox="0 0 24 24" aria-hidden="true">
                <circle cx="11" cy="11" r="6.5" />
                <path d="m16 16 4 4" />
              </svg>
              <input
                type="search"
                value={projectQuery}
                placeholder="Filter projects, branches, or hosts"
                onChange={(event) => setProjectQuery(event.target.value)}
              />
            </label>
            <div className="statusFilters" role="group" aria-label="Filter projects by status">
              {(['all', 'starting', 'running', 'stopped', 'error'] as const).map((status) => (
                <button
                  type="button"
                  className={projectStatusFilter === status ? `active ${status}` : status}
                  aria-pressed={projectStatusFilter === status}
                  key={status}
                  onClick={() => setProjectStatusFilter(status)}
                >
                  <span>{status}</span>
                  <strong>
                    {status === 'all' ? projects.length : projectStatusCounts[status]}
                  </strong>
                </button>
              ))}
            </div>
            <span className="filterResultCount" aria-live="polite">
              {filteredProjects.length} shown
            </span>
          </div>

          <section className={`grid ${projectView}`}>
        {projects.length === 0 && (
          <article className="empty">
            <h2>No projects yet</h2>
            <p>Run <code>porto scan ~/code --depth 3</code>, or restart the daemon to scan Copilot worktrees.</p>
          </article>
        )}
        {projects.length > 0 && filteredProjects.length === 0 && (
          <article className="empty filteredEmpty">
            <h2>No matching projects</h2>
            <p>Try another name or status.</p>
            <button
              type="button"
              onClick={() => {
                setProjectQuery('')
                setProjectStatusFilter('all')
              }}
            >
              Clear filters
            </button>
          </article>
        )}
        {projectGroups.map((group) => (
          <section
            className={`projectGroup ${group.total > 1 ? 'multi' : 'single'}`}
            key={group.key}
          >
            {group.total > 1 && (
              <header className="projectGroupHeader">
                <div>
                  <span>Source project</span>
                  <strong>{group.projects[0].name}</strong>
                </div>
                <small>{group.total} branch runtimes</small>
              </header>
            )}
            <div className="projectGroupCards">
        {group.projects.map((project) => (
          <article className="card" key={project.id}>
            <div className="cardTop">
              <div className="cardHeader">
                <h2>{project.name}</h2>
                <p>{project.path}</p>
              </div>
              <span className={`status ${project.status}`}>{project.status}</span>
            </div>

            <div className="branchControls">
              <BranchPicker
                label="Running branch"
                value={project.branch}
                options={branchOptions[project.id] ?? [project.branch]}
                defaultBranch={project.defaultBranch}
                placeholder="Search branches"
                disabled={branchBusyID === project.id}
                onOpen={() => loadBranches(project)}
                onSelect={(branch) => switchBranch(project, branch)}
              />
              <BranchPicker
                label="New instance"
                value=""
                options={(branchOptions[project.id] ?? []).filter((branch) => branch !== project.branch)}
                defaultBranch={project.defaultBranch}
                placeholder="Search branches"
                disabled={branchBusyID === project.id}
                onOpen={() => loadBranches(project)}
                onSelect={(branch) => createInstance(project, branch)}
              />
            </div>

            <dl>
              <div><dt>Port</dt><dd>{project.port || 'unassigned'}</dd></div>
              <div><dt>PID</dt><dd>{project.pid || '—'}</dd></div>
              <div><dt>Branch</dt><dd>{project.branch}{project.dirty ? ' *' : ''}</dd></div>
              <div><dt>Strategy</dt><dd>{project.strategy}</dd></div>
              <div>
                <dt>HTTPS host</dt>
                <dd>
                  <a href={project.httpsUrl} target="_blank" rel="noreferrer">
                    {project.httpsUrl.replace(/^https:\/\//, '').replace(/\/$/, '')}
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
              <ProjectActionButton
                label="Start project"
                icon="play"
                disabled={setupProjectID === project.id}
                onClick={() => run(String(project.id), 'start')}
              />
              <ProjectActionButton label="Stop project" icon="stop" onClick={() => run(String(project.id), 'stop')} />
              <ProjectActionButton label="Restart project" icon="restart" onClick={() => run(String(project.id), 'restart')} />
              <ProjectActionButton label="Kill project" icon="kill" onClick={() => run(String(project.id), 'kill')} />
              <ProjectActionButton
                className="setupButton"
                label={setupProjectID === project.id ? 'Setting up dependencies' : 'Set up dependencies'}
                icon="setup"
                disabled={project.status === 'running' || project.status === 'starting' || setupProjectID != null}
                onClick={() => setupDependencies(project)}
              />
              <ProjectActionButton
                className="logsButton"
                label="View logs"
                icon="logs"
                onClick={() => viewLogs(project.id)}
              />
              {project.sendboxConfigured && (
                <ProjectActionButton
                  className="sendboxButton"
                  label="Run in Sendbox"
                  icon="sendboxPlay"
                  disabled={
                    !savedSendboxEnabled
                    || sendboxStatus?.state !== 'ready'
                    || project.sendboxStatus === 'running'
                    || project.sendboxStatus === 'stopping'
                  }
                  onClick={() => runSendbox(String(project.id), 'start')}
                />
              )}
              {(project.sendboxConfigured
                || project.sendboxStatus === 'running'
                || project.sendboxStatus === 'stopping') && (
                <ProjectActionButton
                  className="sendboxButton"
                  label="Stop Sendbox"
                  icon="sendboxStop"
                  disabled={project.sendboxStatus !== 'running'}
                  onClick={() => runSendbox(String(project.id), 'stop')}
                />
              )}
              <ProjectActionButton
                className="cleanupButton"
                label="Clean merged branches"
                icon="cleanup"
                disabled={!savedLocalCleanup && !savedRemoteCleanup}
                onClick={() => cleanup(String(project.id))}
              />
              {project.managedInstance && (
                <ProjectActionButton
                  className="removeButton"
                  label="Delete branch instance"
                  icon="remove"
                  disabled={branchBusyID === project.id}
                  onClick={() => removeInstance(project)}
                />
              )}
            </div>
          </article>
        ))}
            </div>
          </section>
        ))}
          </section>
        </>
      )}
    </main>
  )
}

export default App
