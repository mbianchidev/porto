import { useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { formatRelativeTime } from '../format'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { BranchPicker } from '../components/BranchPicker'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { StatusLamp } from '../components/StatusLamp'
import type {
  CleanupResult,
  DockerContainer,
  IntegrationStatus,
  KubernetesPod,
  LogLine,
  Project,
  Settings,
} from '../types'

async function writeClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }
  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  textarea.remove()
  if (!copied) throw new Error('Clipboard access was denied')
}

type ProjectStatusFilter = 'all' | 'starting' | 'running' | 'stopped' | 'error'
type ScanResult = { count: number; projects: Project[] }

function LogConsole({ project, onClose }: { project: Project; onClose: () => void }) {
  const { notifyError, notifyNotice } = useMessages()
  const [stream, setStream] = useState<'all' | 'stdout' | 'stderr'>('all')
  const consoleRef = useRef<HTMLElement>(null)
  const { data, error, loading, reload } = usePolledResource<LogLine[]>(
    (signal) => apiGet(`/api/projects/${project.id}/logs?limit=500&stream=${stream}`, signal),
    2000,
    [project.id, stream],
  )
  const lines = data ?? []

  useEffect(() => {
    const frame = window.requestAnimationFrame(() => {
      consoleRef.current?.scrollIntoView({ behavior: 'smooth', block: 'nearest' })
    })
    return () => window.cancelAnimationFrame(frame)
  }, [])

  async function clearLogs() {
    const label = stream === 'all' ? 'all logs' : `${stream} logs`
    if (!window.confirm(`Clear ${label} for ${project.name}?`)) return
    try {
      const result = await apiSend<{ deleted: number }>(`/api/projects/${project.id}/logs/clear?stream=${stream}`, 'POST')
      notifyNotice('localhost-ing', `Cleared ${result.deleted} ${stream === 'all' ? '' : `${stream} `}log line(s).`)
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Unable to clear logs'))
    }
  }

  async function copyLogs() {
    const text = lines
      .map((line) => `${new Date(line.createdAt).toLocaleTimeString([], { hour12: false })}\t${line.stream}\t${line.line}`)
      .join('\n')
    if (text === '') return
    try {
      await writeClipboard(text)
      notifyNotice('localhost-ing', `Copied ${lines.length} visible log line(s).`)
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Unable to copy logs'))
    }
  }

  return (
    <section ref={consoleRef} className="logConsole" aria-labelledby={`process-console-title-${project.id}`}>
      <div className="consoleHeader">
        <div>
          <h3 id={`process-console-title-${project.id}`}>Process console</h3>
          <p>{project.branch} · {project.status} · {project.pid ? `PID ${project.pid}` : 'no active PID'}</p>
        </div>
        <div className="consoleActions">
          <button type="button" onClick={reload}>Refresh</button>
          <button type="button" disabled={loading || error !== '' || lines.length === 0} onClick={copyLogs}>Copy visible</button>
          <button className="destructiveAction" type="button" onClick={clearLogs}>Clear visible</button>
          <button type="button" onClick={onClose}>Close console</button>
        </div>
      </div>
      <div className="streamTabs" role="tablist" aria-label="Log stream">
        {(['all', 'stdout', 'stderr'] as const).map((option) => (
          <button
            type="button"
            role="tab"
            aria-selected={stream === option}
            className={stream === option ? 'active' : ''}
            key={option}
            onClick={() => setStream(option)}
          >
            {option}
          </button>
        ))}
      </div>
      <div className="logViewport" role="log" aria-live="polite" aria-busy={loading}>
        {error && <div className="logEmpty errorLine">{error}</div>}
        {!error && loading && lines.length === 0 && <div className="logEmpty">Loading process output…</div>}
        {!error && !loading && lines.length === 0 && (
          <div className="logEmpty">No {stream === 'all' ? '' : `${stream} `}output captured yet.</div>
        )}
        {!error && lines.map((line, index) => (
          <div className={`logLine ${line.stream}`} key={`${line.createdAt}-${index}`}>
            <time dateTime={line.createdAt}>{new Date(line.createdAt).toLocaleTimeString([], { hour12: false })}</time>
            <span className="streamLabel">{line.stream}</span>
            <span className="logMessage">{line.line}</span>
          </div>
        ))}
      </div>
    </section>
  )
}

export function LocalhostIng({
  settings,
  sendboxStatus,
  kubeContext,
}: {
  settings: Settings | null
  sendboxStatus: IntegrationStatus | null
  kubeContext: string
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [projectQuery, setProjectQuery] = useState('')
  const [statusFilter, setStatusFilter] = useState<ProjectStatusFilter>('all')
  const [selectedProjectID, setSelectedProjectID] = useState<number | null>(null)
  const [inspectorTab, setInspectorTab] = useState<'overview' | 'logs'>('overview')
  const [branchOptions, setBranchOptions] = useState<Record<number, string[]>>({})
  const [branchBusyID, setBranchBusyID] = useState<number | null>(null)
  const [setupProjectID, setSetupProjectID] = useState<number | null>(null)
  const [addingProject, setAddingProject] = useState(false)
  const [scanRoot, setScanRoot] = useState('')
  const [scanDepth, setScanDepth] = useState(3)
  const [scanning, setScanning] = useState(false)

  const { data, reload } = usePolledResource<Project[]>(
    (signal) => apiGet('/api/projects', signal),
    5000,
    [],
  )
  const dockerDeployments = usePolledResource<DockerContainer[]>(
    (signal) => settings?.dockerEnabled ? apiGet('/api/docker/containers', signal) : Promise.resolve([]),
    5000,
    [settings?.dockerEnabled],
  )
  const kubernetesDeployments = usePolledResource<KubernetesPod[]>(
    (signal) => settings?.kubernetesEnabled
      ? apiGet(`/api/kubernetes/pods?namespace=all&context=${encodeURIComponent(kubeContext)}`, signal)
      : Promise.resolve([]),
    5000,
    [settings?.kubernetesEnabled, kubeContext],
  )
  const projects = data ?? []
  const selectedProject = projects.find((project) => project.id === selectedProjectID) ?? null

  const sourceKey = (project: Project) => project.sourcePath || project.path
  const normalizedQuery = projectQuery.trim().toLocaleLowerCase()
  const matchingSources = new Set(
    projects
      .filter((project) => normalizedQuery === '' || [project.name, project.branch, project.hostname, sourceKey(project)]
        .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
      .map(sourceKey),
  )
  const filteredProjects = projects.filter((project) => {
    const matchesName = matchingSources.has(sourceKey(project))
    const matchesStatus = statusFilter === 'all'
      || project.status === statusFilter
      || (statusFilter === 'error' && !['starting', 'running', 'stopped'].includes(project.status))
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
  const statusCounts = projects.reduce(
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
  const attentionCount = statusCounts.starting + statusCounts.error
  const savedSendboxEnabled = settings?.sendboxEnabled ?? false
  const cleanupEnabled = (settings?.cleanupLocalMerged ?? false) || (settings?.cleanupRemoteMerged ?? false)

  async function run(name: string, verb: string) {
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(name)}/${verb}`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Action failed'))
    }
  }

  function selectProject(id: number, tab: 'overview' | 'logs' = 'overview') {
    setAddingProject(false)
    setSelectedProjectID(id)
    setInspectorTab(tab)
  }

  async function loadBranches(project: Project) {
    if (branchOptions[project.id]) return
    try {
      const result = await apiGet<{ branches: string[] }>(`/api/projects/${project.id}/branches`)
      setBranchOptions((current) => ({ ...current, [project.id]: result.branches }))
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Unable to load branches'))
    }
  }

  async function switchBranch(project: Project, branch: string) {
    if (!branch || branch === project.branch) return
    setBranchBusyID(project.id)
    try {
      await apiSend(`/api/projects/${project.id}/branch`, 'POST', { branch })
      notifyNotice('localhost-ing', `Switched ${project.name} to ${branch}${project.status === 'running' || project.status === 'starting' ? ' and restarted it' : ''}.`)
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Branch switch failed'))
    } finally {
      setBranchBusyID(null)
    }
  }

  async function createInstance(project: Project, branch: string) {
    if (!branch) return
    setBranchBusyID(project.id)
    try {
      await apiSend(`/api/projects/${project.id}/instances`, 'POST', { branch })
      notifyNotice('localhost-ing', `Created and prepared a ${project.name} instance for ${branch}.`)
      setBranchOptions({})
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Unable to create branch instance'))
    } finally {
      setBranchBusyID(null)
    }
  }

  async function removeInstance(project: Project) {
    const dirtyWarning = project.dirty ? ' Uncommitted changes in this instance will be discarded.' : ''
    if (!window.confirm(`Delete the ${project.branch} instance of ${project.name}?${dirtyWarning}`)) return
    setBranchBusyID(project.id)
    try {
      await apiSend(`/api/projects/${project.id}/instance`, 'DELETE')
      if (selectedProjectID === project.id) setSelectedProjectID(null)
      notifyNotice('localhost-ing', `Deleted the ${project.branch} instance of ${project.name}.`)
      setBranchOptions({})
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Unable to remove branch instance'))
    } finally {
      setBranchBusyID(null)
    }
  }

  async function runSendbox(name: string, verb: 'start' | 'stop') {
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(name)}/sendbox/${verb}`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      reload()
      notifyNotice('localhost-ing', verb === 'start' ? 'Sendbox session started.' : 'Stopping Sendbox session.')
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Sendbox action failed'))
    }
  }

  async function cleanup(name: string) {
    try {
      const response = await fetch(`/api/projects/${encodeURIComponent(name)}/cleanup-branches`, { method: 'POST' })
      if (!response.ok) throw new Error(await response.text())
      const result: CleanupResult = await response.json()
      const deleted = [...result.localDeleted.map((b) => `local ${b}`), ...result.remoteDeleted.map((b) => `remote ${b}`)]
      notifyNotice('localhost-ing', deleted.length > 0 ? `Deleted ${deleted.join(', ')}.` : 'No fully merged, unprotected branches found.')
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Branch cleanup failed'))
    }
  }

  async function setupDependencies(project: Project) {
    selectProject(project.id, 'logs')
    setSetupProjectID(project.id)
    try {
      const result = await apiSend<{ commands: string[] }>(`/api/projects/${project.id}/setup`, 'POST')
      notifyNotice('localhost-ing', `Dependency setup completed with ${result.commands.join(' then ')}.`)
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, 'Dependency setup failed'))
    } finally {
      setSetupProjectID(null)
    }
  }

  async function runContainer(container: DockerContainer, verb: 'start' | 'stop' | 'restart') {
    try {
      await apiSend(`/api/docker/containers/${encodeURIComponent(container.id)}/${verb}`, 'POST')
      notifyNotice('localhost-ing', `${container.name} ${verb} requested.`)
      dockerDeployments.reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, `Unable to ${verb} ${container.name}`))
    }
  }

  async function scanForProjects(event: FormEvent) {
    event.preventDefault()
    const root = scanRoot.trim()
    if (root === '') return
    setScanning(true)
    try {
      const result = await apiSend<ScanResult>('/api/scan', 'POST', {
        roots: [root],
        depth: scanDepth,
        ignore: ['.git', 'vendor', 'dist', 'target'],
      })
      notifyNotice(
        'localhost-ing',
        result.count > 0
          ? `Added ${result.count} ${result.count === 1 ? 'deployment' : 'deployments'} from ${root}.`
          : `No runnable projects found under ${root}.`,
      )
      setAddingProject(false)
      setScanRoot('')
      reload()
    } catch (err) {
      notifyError('localhost-ing', errorMessage(err, `Unable to scan ${root}`))
    } finally {
      setScanning(false)
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Fleet status">
        <span className="fleetRailTitle">Fleet signal</span>
        <span className="fleetDatum"><StatusLamp state="running" />Running <strong>{statusCounts.running}</strong></span>
        <span className="fleetDatum"><StatusLamp state="starting" />Starting <strong>{statusCounts.starting}</strong></span>
        <span className="fleetDatum"><StatusLamp state="stopped" />Stopped <strong>{statusCounts.stopped}</strong></span>
        <span className="fleetDatum"><StatusLamp state="crashed" />Fault <strong>{statusCounts.error}</strong></span>
        <span className={`fleetMessage ${attentionCount > 0 ? 'attention' : ''}`}>
          {attentionCount > 0 ? `${attentionCount} ${attentionCount === 1 ? 'channel needs' : 'channels need'} attention` : 'All channels stable'}
        </span>
      </section>

      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter projects</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
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
              className={statusFilter === status ? `active ${status}` : status}
              aria-pressed={statusFilter === status}
              key={status}
              onClick={() => setStatusFilter(status)}
            >
              <span>{status}</span>
              <strong>{status === 'all' ? projects.length : statusCounts[status]}</strong>
            </button>
          ))}
        </div>
        <span className="filterResultCount" aria-live="polite">
          {filteredProjects.length} / {projects.length} channels · {sourceProjectCount} sources
          {managedInstanceCount > 0 && ` · ${managedInstanceCount} instances`}
        </span>
        <div className="toolbarActions">
          <button
            type="button"
            onClick={() => {
              setSelectedProjectID(null)
              setAddingProject(true)
            }}
          >
            Add app
          </button>
          <button className="refreshControl" type="button" onClick={reload}>Refresh</button>
        </div>
      </div>

      <div className="workArea">
        <section className="channelBoard" aria-label="Project channels">
          {projects.length === 0 && (
            <article className="empty">
              <h2>No projects detected</h2>
              <p>Add a source directory here or run <code>porto scan ~/code --depth 3</code>.</p>
              <button type="button" onClick={() => setAddingProject(true)}>Add app</button>
            </article>
          )}
          {projects.length > 0 && filteredProjects.length === 0 && (
            <article className="empty filteredEmpty">
              <h2>No channels match</h2>
              <p>Change the search or status filter to restore the board.</p>
              <button type="button" onClick={() => { setProjectQuery(''); setStatusFilter('all') }}>Clear filters</button>
            </article>
          )}
          {projectGroups.map((group) => (
            <section className={`projectGroup ${group.total > 1 ? 'multi' : 'single'}`} key={group.key}>
              {group.total > 1 && (
                <header className="projectGroupHeader">
                  <div><strong>{group.projects[0].name}</strong><span>Source project</span></div>
                  <small>{group.total} branch channels</small>
                </header>
              )}
              <div className="channelStack">
                {group.projects.map((project) => {
                  const selected = selectedProjectID === project.id
                  return (
                    <article className={`projectChannel ${project.status} ${selected ? 'selected' : ''}`} key={project.id}>
                      <div className="channelFace">
                        <button
                          className="channelToggle"
                          type="button"
                          aria-pressed={selected}
                          onClick={() => selectProject(project.id)}
                        >
                          <StatusLamp state={project.status === 'starting' ? 'starting' : project.status === 'running' ? 'running' : project.status === 'stopped' ? 'stopped' : 'crashed'} />
                          <span className="channelIdentity">
                            <strong>{project.name}</strong>
                            <small>{project.managedInstance ? 'Branch instance' : project.strategy}</small>
                          </span>
                          <span className="channelDatum">
                            <small>Branch</small>
                            <strong>{project.branch}{project.dirty ? ' *' : ''}</strong>
                          </span>
                          <span className="channelDatum channelRoute">
                            <small>Route</small>
                            <strong>{project.hostname || 'host pending'}{project.port ? ` · ${project.port}` : ''}</strong>
                          </span>
                          <span className={`channelState ${project.status}`}>
                            <small>State</small>
                            <strong>{project.status}</strong>
                            <span className="channelUpdated">
                              {project.status === 'running' || project.status === 'starting'
                                ? `up ${formatRelativeTime(project.lastStarted)}`
                                : `updated ${formatRelativeTime(project.updatedAt)}`}
                            </span>
                          </span>
                        </button>
                        <div className="quickActions" aria-label={`${project.name} quick actions`}>
                          <ActionButton label="Start project" icon="play" disabled={setupProjectID === project.id} onClick={() => run(String(project.id), 'start')} />
                          <ActionButton label="Stop project" icon="stop" onClick={() => run(String(project.id), 'stop')} />
                          <ActionButton label="Restart project" icon="restart" onClick={() => run(String(project.id), 'restart')} />
                          <ActionButton
                            label="Open route"
                            icon="open"
                            disabled={!project.httpsUrl}
                            onClick={() => window.open(project.httpsUrl, '_blank', 'noreferrer')}
                          />
                          <ActionButton className="logsButton" label="View logs" icon="logs" onClick={() => selectProject(project.id, 'logs')} />
                        </div>
                      </div>
                    </article>
                  )
                })}
              </div>
            </section>
          ))}
          {(dockerDeployments.data?.length ?? 0) + (kubernetesDeployments.data?.length ?? 0) > 0 && (
            <section className="projectGroup multi runtimeDeployments">
              <header className="projectGroupHeader">
                <div><strong>Runtime deployments</strong><span>Containers and Kubernetes</span></div>
                <small>{(dockerDeployments.data?.length ?? 0) + (kubernetesDeployments.data?.length ?? 0)} workloads</small>
              </header>
              <div className="channelStack">
                {(dockerDeployments.data ?? []).map((container) => (
                  <article className={`projectChannel ${container.state}`} key={`container-${container.id}`}>
                    <div className="channelFace">
                      <a className="channelToggle runtimeDeploymentLink" href="#/containers">
                        <StatusLamp state={container.state === 'running' ? 'running' : container.state === 'paused' ? 'starting' : 'stopped'} />
                        <span className="channelIdentity">
                          <strong>{container.composeService || container.name.replace(/^\//, '')}</strong>
                          <small>{container.composeProject ? `Compose · ${container.composeProject}` : 'Container'}</small>
                        </span>
                        <span className="channelDatum">
                          <small>Image</small>
                          <strong>{container.image}</strong>
                        </span>
                        <span className="channelDatum channelRoute">
                          <small>Ports</small>
                          <strong>{container.ports || 'none published'}</strong>
                        </span>
                        <span className={`channelState ${container.state}`}>
                          <small>State</small>
                          <strong>{container.state}</strong>
                        </span>
                      </a>
                      <div className="quickActions" aria-label={`${container.name} quick actions`}>
                        <ActionButton label="Start container" icon="play" onClick={() => runContainer(container, 'start')} />
                        <ActionButton label="Stop container" icon="stop" onClick={() => runContainer(container, 'stop')} />
                        <ActionButton label="Restart container" icon="restart" onClick={() => runContainer(container, 'restart')} />
                        <a className="iconButton" href="#/containers" aria-label="Inspect container" data-tooltip="Inspect container">
                          <span aria-hidden="true">→</span>
                        </a>
                      </div>
                    </div>
                  </article>
                ))}
                {(kubernetesDeployments.data ?? []).map((pod) => (
                  <article className={`projectChannel ${pod.phase.toLocaleLowerCase()}`} key={`pod-${pod.namespace}-${pod.name}`}>
                    <div className="channelFace">
                      <a className="channelToggle runtimeDeploymentLink" href="#/pods">
                        <StatusLamp state={pod.phase === 'Running' ? 'running' : pod.phase === 'Pending' ? 'starting' : 'crashed'} />
                        <span className="channelIdentity">
                          <strong>{pod.name}</strong>
                          <small>Kubernetes pod</small>
                        </span>
                        <span className="channelDatum">
                          <small>Namespace</small>
                          <strong>{pod.namespace}</strong>
                        </span>
                        <span className="channelDatum channelRoute">
                          <small>Node</small>
                          <strong>{pod.node || 'not scheduled'}</strong>
                        </span>
                        <span className={`channelState ${pod.phase.toLocaleLowerCase()}`}>
                          <small>State</small>
                          <strong>{pod.phase}</strong>
                        </span>
                      </a>
                      <div className="quickActions">
                        <a className="buttonLink" href="#/pods">Inspect pod</a>
                      </div>
                    </div>
                  </article>
                ))}
              </div>
            </section>
          )}
        </section>

        {selectedProject && (
          <Inspector
            title={`${selectedProject.name} service channel`}
            subtitle={selectedProject.path}
            onClose={() => setSelectedProjectID(null)}
          >
            <InspectorTabs
              tabs={[{ id: 'overview', label: 'Overview' }, { id: 'logs', label: 'Logs' }]}
              activeID={inspectorTab}
              onSelect={(id) => setInspectorTab(id as 'overview' | 'logs')}
            />
            {inspectorTab === 'overview' && (
              <>
                <div className="drawerReadouts" aria-label="Runtime readouts">
                  <span><small>PID</small><strong>{selectedProject.pid || '—'}</strong></span>
                  <span><small>Port</small><strong>{selectedProject.port || '—'}</strong></span>
                  <span><small>Strategy</small><strong>{selectedProject.strategy}</strong></span>
                </div>

                <div className="drawerGrid">
                  <section className="drawerPanel" aria-labelledby={`routing-${selectedProject.id}`}>
                    <h3 id={`routing-${selectedProject.id}`}>Routing and runtime</h3>
                    <dl className="runtimeGrid">
                      <div><dt>Branch</dt><dd>{selectedProject.branch}{selectedProject.dirty ? ' · modified' : ''}</dd></div>
                      <div>
                        <dt>HTTPS route</dt>
                        <dd>
                          <a href={selectedProject.httpsUrl} target="_blank" rel="noreferrer">
                            {selectedProject.httpsUrl.replace(/^https:\/\//, '').replace(/\/$/, '')}
                          </a>
                        </dd>
                      </div>
                      <div>
                        <dt>Sendbox</dt>
                        <dd className={`sendboxState ${selectedProject.sendboxStatus}`} title={selectedProject.sendboxMessage}>
                          {selectedProject.sendboxConfigured ? selectedProject.sendboxStatus : 'not configured'}
                        </dd>
                      </div>
                      <div><dt>Source</dt><dd>{selectedProject.sourcePath || selectedProject.path}</dd></div>
                    </dl>
                  </section>

                  <section className="drawerPanel" aria-labelledby={`branches-${selectedProject.id}`}>
                    <h3 id={`branches-${selectedProject.id}`}>Branch routing</h3>
                    <div className="branchControls">
                      <BranchPicker
                        label="Running branch"
                        value={selectedProject.branch}
                        options={branchOptions[selectedProject.id] ?? [selectedProject.branch]}
                        defaultBranch={selectedProject.defaultBranch}
                        placeholder="Search branches"
                        disabled={branchBusyID === selectedProject.id}
                        onOpen={() => loadBranches(selectedProject)}
                        onSelect={(branch) => switchBranch(selectedProject, branch)}
                      />
                      <BranchPicker
                        label="New instance"
                        value=""
                        options={(branchOptions[selectedProject.id] ?? []).filter((branch) => branch !== selectedProject.branch)}
                        defaultBranch={selectedProject.defaultBranch}
                        placeholder="Search branches"
                        disabled={branchBusyID === selectedProject.id}
                        onOpen={() => loadBranches(selectedProject)}
                        onSelect={(branch) => createInstance(selectedProject, branch)}
                      />
                    </div>
                  </section>
                </div>

                <div className="commandStrip">
                  <span>Launch command</span>
                  <code>{selectedProject.command}</code>
                </div>

                <div className="maintenanceBar">
                  <span>Maintenance controls</span>
                  <div className="actions">
                    <ActionButton label="Kill project" icon="kill" onClick={() => run(String(selectedProject.id), 'kill')} />
                    <ActionButton
                      className="setupButton"
                      label={setupProjectID === selectedProject.id ? 'Setting up dependencies' : 'Set up dependencies'}
                      icon="setup"
                      disabled={selectedProject.status === 'running' || selectedProject.status === 'starting' || setupProjectID != null}
                      onClick={() => setupDependencies(selectedProject)}
                    />
                    {selectedProject.sendboxConfigured && (
                      <ActionButton
                        className="sendboxButton"
                        label="Run in Sendbox"
                        icon="sendboxPlay"
                        disabled={!savedSendboxEnabled || sendboxStatus?.state !== 'ready' || selectedProject.sendboxStatus === 'running' || selectedProject.sendboxStatus === 'stopping'}
                        onClick={() => runSendbox(String(selectedProject.id), 'start')}
                      />
                    )}
                    {(selectedProject.sendboxConfigured || selectedProject.sendboxStatus === 'running' || selectedProject.sendboxStatus === 'stopping') && (
                      <ActionButton
                        className="sendboxButton"
                        label="Stop Sendbox"
                        icon="sendboxStop"
                        disabled={selectedProject.sendboxStatus !== 'running'}
                        onClick={() => runSendbox(String(selectedProject.id), 'stop')}
                      />
                    )}
                    <ActionButton
                      className="cleanupButton"
                      label="Clean merged branches"
                      icon="cleanup"
                      disabled={!cleanupEnabled}
                      onClick={() => cleanup(String(selectedProject.id))}
                    />
                    {selectedProject.managedInstance && (
                      <ActionButton
                        className="removeButton"
                        label="Delete branch instance"
                        icon="remove"
                        disabled={branchBusyID === selectedProject.id}
                        onClick={() => removeInstance(selectedProject)}
                      />
                    )}
                  </div>
                </div>
              </>
            )}
            {inspectorTab === 'logs' && (
              <LogConsole project={selectedProject} onClose={() => setInspectorTab('overview')} />
            )}
          </Inspector>
        )}

        {addingProject && (
          <Inspector title="Add to localhost-ing" subtitle="Discover runnable projects from an existing source directory." onClose={() => setAddingProject(false)}>
            <section className="drawerPanel">
              <h3>Project discovery</h3>
              <p className="hintLine">
                Porto detects Make, Docker Compose, Node.js, Python, Go, and Rust projects without rewriting their configuration.
              </p>
              <form className="inspectorForm" onSubmit={scanForProjects}>
                <label>
                  <span>Source directory</span>
                  <input
                    type="text"
                    value={scanRoot}
                    placeholder="~/code/my-service"
                    autoFocus
                    onChange={(event) => setScanRoot(event.target.value)}
                    required
                  />
                </label>
                <label>
                  <span>Scan depth</span>
                  <input
                    type="number"
                    min={1}
                    max={12}
                    value={scanDepth}
                    onChange={(event) => setScanDepth(Number(event.target.value))}
                  />
                </label>
                <button type="submit" disabled={scanning || scanRoot.trim() === ''}>
                  {scanning ? 'Scanning…' : 'Discover deployments'}
                </button>
              </form>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
