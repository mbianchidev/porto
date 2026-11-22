import { lazy, Suspense, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react'
import { apiGet, apiSend, errorMessage, isAbortError } from '../api'
import { useContainerSnapshots } from '../containerSnapshots'
import {
  composeStackPlan,
  composeStackResources,
  containerDisplayName,
  containerState,
  describeStepFailures,
  isActive,
  isPaused,
  isStartable,
  removeComposeStack,
  runComposeSteps,
  type ComposeStackAction,
  type ComposeStackResources,
} from '../containerLifecycle'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { useDockerMode } from '../dockerMode'
import { ActionButton } from '../components/ActionButton'
import { DiveTerminal } from '../components/DiveTerminal'
import { RuntimeFiles } from '../components/RuntimeFiles'
import { ContainerResourceSaver } from '../components/ResourceManagement'
import { Inspector, InspectorErrorBoundary, InspectorTabs } from '../components/Inspector'
import { ExtensionContributions } from '../components/ResourceExtensions'
import { InventoryList, type InventoryColumn } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import { ResourceInput } from '../components/ResourceInput'
import type {
  DockerContainer,
  DockerContainerAction,
  DockerContainerCreateRequest,
  DockerContainerCreateResult,
  DockerImage,
  DockerStatus,
  LampState,
} from '../types'

const COLUMNS_TEMPLATE = '12px minmax(180px,1.3fr) minmax(160px,1fr) minmax(150px,1fr) minmax(120px,0.8fr)'
const EMPTY_CREATE_DRAFT = {
  name: '',
  image: '',
  hostPort: '',
  containerPort: '',
  healthCommand: '',
  platform: '',
  architecturePolicy: '',
}
const InteractiveTerminal = lazy(async () => {
  const terminal = await import('../components/VMTerminal')
  return { default: terminal.InteractiveTerminal }
})
const ContainerLogs = lazy(async () => {
  const inspector = await import('../components/ContainerDataInspector')
  return { default: inspector.ContainerLogs }
})
const ContainerStats = lazy(async () => {
  const inspector = await import('../components/ContainerDataInspector')
  return { default: inspector.ContainerStats }
})
const ContainerInspect = lazy(async () => {
  const inspector = await import('../components/ContainerDataInspector')
  return { default: inspector.ContainerInspect }
})
const TERMINAL_SHELLS = ['sh', 'bash', 'ash', '/bin/bash'] as const
const WINDOWS_TERMINAL_SHELLS = ['cmd.exe', 'powershell.exe'] as const
type TerminalShell = (typeof TERMINAL_SHELLS)[number] | (typeof WINDOWS_TERMINAL_SHELLS)[number]
const STACK_ACTION_TEXT: Record<ComposeStackAction, { label: string; progress: string; done: string }> = {
  start: { label: 'Start stack', progress: 'Starting', done: 'Started' },
  stop: { label: 'Stop stack', progress: 'Stopping', done: 'Stopped' },
  restart: { label: 'Restart stack', progress: 'Restarting', done: 'Restarted' },
}

function containerTerminalSocketURL(container: DockerContainer, shell: TerminalShell, debug: boolean): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const params = new URLSearchParams({ shell, debug: String(debug) })
  return `${protocol}//${window.location.host}/api/docker/containers/${encodeURIComponent(container.id)}/terminal?${params.toString()}`
}

function taskLabel(container: DockerContainer) {
  if (!container.taskPresent) return container.state === 'running' ? 'Running' : 'No task'
  return container.pid ? `PID ${container.pid}` : 'Running'
}

function healthLabel(status: string) {
  return status === 'disabled' ? 'Not configured' : status
}

function containerMatches(container: DockerContainer, query: string) {
  return query === '' || [
    container.name,
    container.id,
    container.image,
    container.status,
    container.composeProject ?? '',
    container.composeService ?? '',
  ].some((value) => value.toLocaleLowerCase().includes(query))
}

function composeGroupLamp(containers: DockerContainer[]): LampState {
  const states = containers.map((container) => lampStateFor(container.state))
  for (const state of ['crashed', 'starting', 'running', 'stopped', 'neutral'] as const) {
    if (states.includes(state)) return state
  }
  return 'neutral'
}

function containerColumns(grouped: boolean): InventoryColumn<DockerContainer>[] {
  return [
    {
      header: grouped ? 'Service / container' : 'Name',
      render: (container) => (
        <span className="containerIdentity">
          <strong>{grouped ? container.composeService || containerDisplayName(container) : containerDisplayName(container)}</strong>
          {grouped && container.composeService && <small>{containerDisplayName(container)}</small>}
        </span>
      ),
    },
    { header: 'Image', className: 'mono', render: (container) => container.image },
    { header: 'Status', className: 'mono', render: (container) => container.status },
    { header: 'Ports', className: 'mono', render: (container) => container.ports || '—' },
  ]
}

export function Containers() {
  const dockerMode = useDockerMode()
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [createDraft, setCreateDraft] = useState(EMPTY_CREATE_DRAFT)
  const [containerTab, setContainerTab] = useState('overview')
  const inspectorTrigger = useRef<HTMLElement | null>(null)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const containers = useContainerSnapshots(status.data?.enabled ?? false)
  const items = containers.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const standaloneContainers = items.filter((container) => !container.composeProject && containerMatches(container, normalizedQuery))
  const composeProjects = new Map<string, DockerContainer[]>()
  for (const container of items) {
    if (!container.composeProject) continue
    const projectContainers = composeProjects.get(container.composeProject) ?? []
    projectContainers.push(container)
    composeProjects.set(container.composeProject, projectContainers)
  }
  const composeGroups = [...composeProjects.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([project, projectContainers]) => ({
      project,
      stack: projectContainers,
      containers: project.toLocaleLowerCase().includes(normalizedQuery)
        ? projectContainers
        : projectContainers.filter((container) => containerMatches(container, normalizedQuery)),
    }))
    .filter((group) => group.containers.length > 0)
  const filteredCount = standaloneContainers.length + composeGroups.reduce((count, group) => count + group.containers.length, 0)
  const selected = items.find((container) => container.id === selectedID) ?? null
  const runtimeAvailable = containers.snapshot?.available ?? status.data?.available ?? false
  const stale = Boolean(containers.snapshot?.stale || status.data?.stale || containers.error || status.error)
  const directCapabilities = containers.snapshot?.capabilities
  const directFeatureList = directCapabilities
    ? [
        directCapabilities.directCreation,
        directCapabilities.execLifecycle,
        directCapabilities.healthUpdates,
        directCapabilities.networkUpdates,
        directCapabilities.checkpointRestore,
      ]
    : []
  const reportedFeatures = directFeatureList.filter((capability) => capability && typeof capability.supported === 'boolean')
  const invalidCapabilities = reportedFeatures.length !== directFeatureList.length
  const available = runtimeAvailable && !invalidCapabilities
  const supportedDirectFeatures = reportedFeatures.filter((capability) => capability.supported).length
  const directCapabilityDetail = reportedFeatures
    .filter((capability) => !capability.supported && capability.reason)
    .map((capability) => capability.reason)
    .join(' · ')
  const images = usePolledResource<DockerImage[]>(
    (signal) => available ? apiGet('/api/docker/images', signal) : Promise.resolve([]),
    15000,
    [available],
    available ? 'docker:images:create' : undefined,
  )
  const imageOptions = [...new Set((images.data ?? [])
    .filter((image) => image.repository && image.repository !== '<none>')
    .map((image) => image.tag && image.tag !== '<none>' ? `${image.repository}:${image.tag}` : image.repository))]

  function openContainer(container: DockerContainer, tab: 'overview' | 'terminal' | 'layers' = 'overview') {
    if (document.activeElement instanceof HTMLElement && !document.activeElement.closest('.inspector')) {
      inspectorTrigger.current = document.activeElement
    }
    setSelectedID(container.id)
    setContainerTab(tab)
  }

  function closeInspector() {
    setSelectedID(null)
    requestAnimationFrame(() => {
      if (inspectorTrigger.current?.isConnected) inspectorTrigger.current.focus()
    })
  }

  async function createContainer(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const hostPort = Number.parseInt(createDraft.hostPort, 10) || 0
    const containerPort = Number.parseInt(createDraft.containerPort, 10) || 0
    const request: DockerContainerCreateRequest = {
      name: createDraft.name.trim(),
      image: createDraft.image.trim(),
      hostPort,
      containerPort,
      healthCommand: createDraft.healthCommand.trim(),
      ...(createDraft.platform ? { platform: createDraft.platform } : {}),
      ...(createDraft.architecturePolicy ? { architecturePolicy: createDraft.architecturePolicy } : {}),
    }
    setCreating(true)
    try {
      const result = await apiSend<DockerContainerCreateResult>('/api/docker/containers', 'POST', request)
      notifyNotice('containers', `Created and started ${result.name}.`)
      setCreateDraft(EMPTY_CREATE_DRAFT)
      setCreateOpen(false)
      setSelectedID(result.id)
      containers.reload()
    } catch (err) {
      notifyError('containers', errorMessage(err, 'Unable to create container'))
    } finally {
      setCreating(false)
    }
  }

  async function containerAction(container: DockerContainer, action: DockerContainerAction, confirmMessage?: string) {
    if (confirmMessage && !window.confirm(confirmMessage)) return
    try {
      await apiSend(`/api/docker/containers/${container.id}/${action}`, 'POST')
      notifyNotice('containers', `${action} succeeded for ${container.name}.`)
      containers.reload()
      if (action.startsWith('remove') && selectedID === container.id) setSelectedID(null)
    } catch (err) {
      notifyError('containers', errorMessage(err, `Unable to ${action} ${container.name}`))
    }
  }

  function stackChanged(removedIDs: string[] = []) {
    containers.reload()
    if (selectedID && removedIDs.includes(selectedID)) setSelectedID(null)
  }

  function containerActions(container: DockerContainer) {
    const paused = isPaused(container)
    const startable = isStartable(container)
    const active = isActive(container)
    return (
      <>
        <ActionButton label="Inspect image layers" icon="terminal" disabled={dockerMode === 'windows'} onClick={() => openContainer(container, 'layers')} />
        <ActionButton label="Start container" icon="play" disabled={!startable} onClick={() => containerAction(container, 'start')} />
        <ActionButton label="Resume container" icon="play" disabled={!paused} onClick={() => containerAction(container, 'unpause')} />
        <ActionButton label="Stop container" icon="stop" disabled={!active} onClick={() => containerAction(container, 'stop')} />
        <ActionButton label="Restart container" icon="restart" disabled={!active} onClick={() => containerAction(container, 'restart')} />
        <ActionButton
          className="removeButton"
          label={startable ? 'Remove container' : 'Remove container (stop it first)'}
          icon="remove"
          disabled={!startable}
          onClick={() => containerAction(container, 'remove', `Remove container ${container.name}?`)}
        />
        <ActionButton
          className="removeButton"
          label="Force remove container"
          icon="kill"
          onClick={() => containerAction(container, 'remove-force', `Force-remove container ${container.name} even if running?`)}
        />
      </>
    )
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Docker signal</span>
        <span className="fleetDatum">
          <StatusLamp state={stale ? 'starting' : available ? 'running' : 'crashed'} />
          {stale ? 'Stale' : available ? 'Live' : 'Unavailable'}
        </span>
        {status.data?.context && <span className="fleetDatum"><small>Context</small><strong>{status.data.context}</strong></span>}
        {containers.snapshot?.namespace && <span className="fleetDatum"><small>Namespace</small><strong>{containers.snapshot.namespace}</strong></span>}
        {directFeatureList.length > 0 && (
          <span className="fleetDatum" title={directCapabilityDetail || 'All direct runtime features are available'}>
            <small>Direct</small><strong>{supportedDirectFeatures}/{directFeatureList.length}</strong>
          </span>
        )}
        <span className="fleetMessage">
          {items.length} container(s) · revision {containers.snapshot?.revision ?? 0}
          {!containers.connected && available ? ' · reconnecting updates' : ''}
        </span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter containers</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <ResourceInput options={items.flatMap((container) => [containerDisplayName(container), ...(container.composeProject ? [container.composeProject] : [])])} type="search" value={query} placeholder="Filter containers by name, image, or status" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filteredCount} / {items.length} containers</span>
        <button className="refreshControl" type="button" disabled={!available} onClick={() => setCreateOpen((value) => !value)}>
          {createOpen ? 'Close creator' : 'Create container'}
        </button>
        <button className="refreshControl" type="button" onClick={containers.reload}>Refresh</button>
      </div>
      {createOpen && available && (
        <section className="drawerPanel containerCreatePanel" aria-labelledby="create-container-title">
          <div className="drawerPanelHeading">
            <div>
              <h3 id="create-container-title">Create and start a container</h3>
              <p>Use a local image name or a remote image reference. Remote images are pulled automatically.</p>
            </div>
          </div>
          <form className="containerCreateForm" onSubmit={createContainer}>
            <label>
              <span>Name</span>
              <input
                type="text"
                required
                value={createDraft.name}
                placeholder="porto-nginx"
                onChange={(event) => setCreateDraft({ ...createDraft, name: event.target.value })}
              />
            </label>
            <label className="containerImageField">
              <span>Local or remote image</span>
              <input
                type="text"
                required
                list="container-image-options"
                value={createDraft.image}
                placeholder="nginx:alpine"
                onChange={(event) => setCreateDraft({ ...createDraft, image: event.target.value })}
              />
              <datalist id="container-image-options">
                {imageOptions.map((image) => <option value={image} key={image} />)}
              </datalist>
            </label>
            <label>
              <span>Host port</span>
              <input
                type="number"
                min="1"
                max="65535"
                value={createDraft.hostPort}
                placeholder="8080"
                onChange={(event) => setCreateDraft({ ...createDraft, hostPort: event.target.value })}
              />
            </label>
            <label>
              <span>Container port</span>
              <input
                type="number"
                min="1"
                max="65535"
                value={createDraft.containerPort}
                placeholder="80"
                onChange={(event) => setCreateDraft({ ...createDraft, containerPort: event.target.value })}
              />
            </label>
            <label className="containerHealthField">
              <span>Health command <small>optional</small></span>
              <input
                type="text"
                value={createDraft.healthCommand}
                placeholder="wget -q -O /dev/null http://127.0.0.1/"
                onChange={(event) => setCreateDraft({ ...createDraft, healthCommand: event.target.value })}
              />
              <small>Runs inside the container every 30 seconds. Leave blank when the image supplies no suitable command.</small>
            </label>
            {dockerMode !== 'windows' && <>
              <label>
                <span>Linux platform</span>
                <select value={createDraft.platform} onChange={(event) => setCreateDraft({ ...createDraft, platform: event.target.value })}>
                  <option value="">Backend native</option>
                  <option value="linux/arm64">linux/arm64</option>
                  <option value="linux/amd64">linux/amd64</option>
                </select>
              </label>
              <label>
                <span>Architecture policy override</span>
                <select value={createDraft.architecturePolicy} onChange={(event) => setCreateDraft({ ...createDraft, architecturePolicy: event.target.value })}>
                  <option value="">Use global policy</option>
                  <option value="automatic">Automatic</option>
                  <option value="native-only">Native only</option>
                  <option value="rosetta-preferred">Prefer Rosetta</option>
                  <option value="qemu-compatible">QEMU compatible</option>
                </select>
              </label>
            </>}
            <div className="containerCreateActions">
              <button type="submit" disabled={creating}>{creating ? 'Creating…' : 'Create and start'}</button>
              <button type="button" disabled={creating} onClick={() => { setCreateOpen(false); setCreateDraft(EMPTY_CREATE_DRAFT) }}>Cancel</button>
            </div>
          </form>
        </section>
      )}
      <div className="workArea">
        {!available ? (
          <RuntimeGate
            label="Docker"
            enabled={status.data?.enabled ?? false}
            message={invalidCapabilities ? 'The selected endpoint returned an incomplete capability report. Verify its supported Porto agent protocol and refresh; controls are unavailable and no local fallback was used.' : containers.snapshot?.message || containers.error || status.data?.message || status.error || (status.data?.enabled ? 'Confirm the configured Docker-compatible engine is running.' : undefined)}
          />
        ) : (
          <div className="containerInventoryGroups">
            {composeGroups.map((group) => (
              <ComposeContainerGroup
                project={group.project}
                containers={group.containers}
                stackContainers={group.stack}
                selectedID={selectedID}
                onSelect={(container) => openContainer(container)}
                onStackChanged={stackChanged}
                renderActions={containerActions}
                key={group.project}
              />
            ))}
            {standaloneContainers.length > 0 && (
              <section
                className="standaloneContainerGroup"
                aria-label={composeGroups.length === 0 ? 'Standalone containers' : undefined}
                aria-labelledby={composeGroups.length > 0 ? 'standalone-containers-title' : undefined}
              >
                {composeGroups.length > 0 && <h2 className="sectionSubhead" id="standalone-containers-title">Standalone containers</h2>}
                <InventoryList
                  items={standaloneContainers}
                  getKey={(container) => container.id}
                  columnsTemplate={COLUMNS_TEMPLATE}
                  getLamp={(container) => lampStateFor(container.state)}
                  getLampLabel={(container) => container.state}
                  selectedKey={selectedID}
                  onSelect={(container) => openContainer(container)}
                  ariaLabel="Standalone Docker containers"
                  emptyMessage={containers.error || 'No standalone containers found.'}
                  columns={containerColumns(false)}
                  renderActions={containerActions}
                />
              </section>
            )}
            {filteredCount === 0 && <article className="empty"><p>{containers.error || 'No containers found.'}</p></article>}
          </div>
        )}

        {selected && (
          <Inspector title={containerDisplayName(selected)} subtitle={[selected.image, selected.composeProject, selected.composeService].filter(Boolean).join(' · ')} onClose={closeInspector}>
            <InspectorTabs
              panelID="container-inspector-panel"
              tabs={[
                { id: 'overview', label: 'Overview' },
                { id: 'logs', label: 'Logs' },
                { id: 'files', label: 'Files' },
                { id: 'stats', label: 'Stats' },
                { id: 'inspect', label: 'Inspect' },
                { id: 'terminal', label: 'Terminal' },
                { id: 'layers', label: 'Layers' },
              ]}
              activeID={containerTab}
              onSelect={setContainerTab}
            />
            <div role="tabpanel" id="container-inspector-panel" aria-labelledby={`container-inspector-panel-${containerTab}`} tabIndex={0}>
            {containerTab === 'overview' && dockerMode !== 'windows' && <ContainerResourceSaver key={selected.id} container={selected} onChanged={containers.reload} />}
            {containerTab === 'overview' && (
              <>
                <ExtensionContributions resourceKind="container" resourceID={selected.id} />
                <div className="drawerReadouts" aria-label="Container readouts">
                  <span><small>State</small><strong>{selected.state}</strong></span>
                  <span><small>Task</small><strong>{taskLabel(selected)}</strong></span>
                  <span><small>Health</small><strong>{healthLabel(selected.health.status)}</strong></span>
                  <span><small>Execution</small><strong>{selected.execution || 'not recorded'}{selected.platform ? ` / ${selected.platform}` : ''}</strong></span>
                  <span><small>Restarts</small><strong>{selected.restartCount}</strong></span>
                  <span><small>Created</small><strong>{selected.createdAt || '—'}</strong></span>
                </div>
                <section className="drawerPanel">
                  <h3>Runtime detail</h3>
                  <dl className="runtimeGrid">
                    <div><dt>Status</dt><dd>{selected.status}</dd></div>
                    <div><dt>Ports</dt><dd>{selected.ports || '—'}</dd></div>
                    <div><dt>Networks</dt><dd>{selected.networks || '—'}</dd></div>
                    <div><dt>Mounts</dt><dd>{selected.mounts || '—'}</dd></div>
                    <div><dt>Exit</dt><dd>{selected.exitCode === undefined ? '—' : `${selected.exitCode}${selected.exitSignal ? ` (signal ${selected.exitSignal})` : ''}`}</dd></div>
                    <div><dt>Exit reason</dt><dd>{selected.exitReason || '—'}</dd></div>
                    <div><dt>Restart policy</dt><dd>{selected.restartPolicy || 'none'}</dd></div>
                    <div><dt>Last restart</dt><dd>{selected.lastRestartReason ? `${selected.lastRestartReason}${selected.lastRestartAt ? ` · ${selected.lastRestartAt}` : ''}` : '—'}</dd></div>
                    <div><dt>CPU quota</dt><dd>{selected.resources.cpuQuota ?? '—'}</dd></div>
                    <div><dt>Memory limit</dt><dd>{selected.resources.memoryLimit ?? '—'}</dd></div>
                    <div><dt>Health output</dt><dd>{selected.health.output || '—'}</dd></div>
                    <div>
                      <dt>Stop behavior</dt>
                      <dd>
                        {selected.stopSignal || 'default signal'} / {selected.stopTimeout === undefined ? 'default timeout' : `${selected.stopTimeout}s`}
                      </dd>
                    </div>
                  </dl>
                  {selected.inventoryError && <p className="errorText">{selected.inventoryError}</p>}
                </section>
                {selected.networkDetails && selected.networkDetails.length > 0 && (
                  <section className="drawerPanel">
                    <h3>Network endpoints</h3>
                    <dl className="runtimeGrid">
                      {selected.networkDetails.map((network, index) => (
                        <div key={`${network.name}-${network.interface || index}`}>
                          <dt>{network.name || 'port mapping'}</dt>
                          <dd>
                            {[
                              network.interface,
                              network.ipAddress,
                              network.gateway ? `via ${network.gateway}` : '',
                              network.aliases?.length ? `aliases ${network.aliases.join(', ')}` : '',
                              network.hostPort ? `${network.hostIp || '0.0.0.0'}:${network.hostPort}->${network.containerPort}/${network.protocol || 'tcp'}` : '',
                            ].filter(Boolean).join(' · ') || 'Attached'}
                          </dd>
                        </div>
                      ))}
                    </dl>
                  </section>
                )}
                {selected.history && selected.history.length > 0 && (
                  <section className="drawerPanel">
                    <h3>Lifecycle history</h3>
                    <dl className="runtimeGrid">
                      {selected.history.slice(-6).reverse().map((event) => (
                        <div key={event.sequence}>
                          <dt>{event.type}</dt>
                          <dd>{event.reason || event.topic} · {event.timestamp}</dd>
                        </div>
                      ))}
                    </dl>
                  </section>
                )}
                <div className="maintenanceBar">
                  <span>Container controls</span>
                  <div className="actions">
                    {containerActions(selected)}
                    <ActionButton label="Pause container" icon="pause" disabled={containerState(selected) !== 'running'} onClick={() => containerAction(selected, 'pause')} />
                  </div>
                </div>
              </>
            )}
            {containerTab === 'terminal' && <ContainerTerminal container={selected} />}
            {containerTab === 'files' && (dockerMode === 'linux'
              ? <RuntimeFiles key={selected.id} kind="container" name={selected.id} />
              : <p role="status">The Linux file helper is not used for native Windows containers. Use explicitly selected Windows bind mounts and container exec.</p>)}
            <InspectorErrorBoundary key={`${selected.id}:${containerTab}`} resourceName="Container">
              <Suspense fallback={<p role="status">Loading container data view…</p>}>
                {containerTab === 'logs' && <ContainerLogs key={selected.id} id={selected.id} />}
                {containerTab === 'stats' && (dockerMode === 'linux'
                  ? <ContainerStats key={selected.id} id={selected.id} running={containerState(selected) === 'running'} />
                  : <p role="status">Linux cgroup metrics are not advertised for native Windows containers.</p>)}
                {containerTab === 'inspect' && <ContainerInspect key={selected.id} id={selected.id} />}
              </Suspense>
            </InspectorErrorBoundary>
            {containerTab === 'layers' && <DiveTerminal image={selected.image} />}
            </div>
          </Inspector>
        )}
      </div>
    </>
  )
}

function ComposeContainerGroup({
  project,
  containers,
  stackContainers,
  selectedID,
  onSelect,
  onStackChanged,
  renderActions,
}: {
  project: string
  containers: DockerContainer[]
  stackContainers: DockerContainer[]
  selectedID: string | null
  onSelect: (container: DockerContainer) => void
  onStackChanged: (removedIDs?: string[]) => void
  renderActions: (container: DockerContainer) => ReactNode
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [open, setOpen] = useState(true)
  const [busy, setBusy] = useState('')
  const [confirmingRemoval, setConfirmingRemoval] = useState(false)
  const [resources, setResources] = useState<ComposeStackResources | null>(null)
  const [deleteVolumes, setDeleteVolumes] = useState(false)
  const resourceRequest = useRef<AbortController | null>(null)
  const removeTrigger = useRef<HTMLButtonElement | null>(null)
  const removalPanel = useRef<HTMLElement | null>(null)
  const baseID = useId()
  const bodyID = `${baseID}-body`
  const removalTitleID = `${baseID}-removal-title`
  const removalPanelID = `${baseID}-removal`
  const services = new Set(containers.map((container) => container.composeService).filter(Boolean)).size
  const running = containers.filter((container) => containerState(container) === 'running').length
  const groupState = composeGroupLamp(containers)
  const activeCount = stackContainers.filter(isActive).length
  const startable = stackContainers.some((container) => isStartable(container) || isPaused(container))

  useEffect(() => () => resourceRequest.current?.abort(), [])
  useEffect(() => {
    if (confirmingRemoval) removalPanel.current?.focus()
  }, [confirmingRemoval])

  async function runStackAction(action: ComposeStackAction) {
    const text = STACK_ACTION_TEXT[action]
    const steps = composeStackPlan(stackContainers, action)
    const total = steps.flat().length
    setBusy(`${text.progress} stack…`)
    try {
      const result = await runComposeSteps(steps, action !== 'stop')
      if (result.failures.length === 0) {
        notifyNotice('containers', `${text.done} ${result.completed.length} container(s) in ${project}.`)
      } else {
        const skipped = result.skipped.length > 0 ? ` Skipped ${result.skipped.length} dependent container(s).` : ''
        notifyError('containers', `Unable to ${action} ${result.failures.length} of ${total} container(s) in ${project}. ${describeStepFailures(result.failures)}.${skipped}`)
      }
    } finally {
      setBusy('')
      onStackChanged()
    }
  }

  function openRemoval() {
    resourceRequest.current?.abort()
    const controller = new AbortController()
    resourceRequest.current = controller
    setOpen(true)
    setConfirmingRemoval(true)
    setDeleteVolumes(false)
    setResources(null)
    composeStackResources(project, controller.signal)
      .then((value) => {
        if (!controller.signal.aborted) setResources(value)
      })
      .catch((err: unknown) => {
        if (isAbortError(err)) return
        const message = errorMessage(err, 'Unable to list stack resources')
        setResources({ networks: [], volumes: [], networkError: message, volumeError: message })
      })
  }

  function closeRemoval() {
    resourceRequest.current?.abort()
    setConfirmingRemoval(false)
    setResources(null)
    setDeleteVolumes(false)
    requestAnimationFrame(() => {
      if (removeTrigger.current?.isConnected) removeTrigger.current.focus()
    })
  }

  async function removeStack() {
    if (!resources) return
    const withVolumes = deleteVolumes && !resources.volumeError
    const keptNetworks = resources.networkError ? ' Networks could not be listed and were kept.' : ''
    setBusy('Stopping…')
    try {
      const removal = await removeComposeStack(stackContainers, resources, withVolumes, (phase) => setBusy(`${phase}…`))
      const failures = [...removal.stop.failures, ...removal.remove.failures]
      if (failures.length > 0) {
        notifyError('containers', `Removed ${removal.remove.completed.length} of ${stackContainers.length} container(s) in ${project}. ${describeStepFailures(failures)}. Networks and volumes were kept because containers remain.`)
      } else {
        const parts = [`${removal.remove.completed.length} container(s)`, `${removal.removedNetworks.length} network(s)`]
        if (withVolumes) parts.push(`${removal.removedVolumes.length} volume(s)`)
        if (removal.resourceFailures.length > 0) {
          notifyError('containers', `Removed ${parts.join(', ')} from ${project}, but some resources remain. ${removal.resourceFailures.join('; ')}.${keptNetworks}`)
        } else {
          notifyNotice('containers', `Removed Compose stack ${project}: ${parts.join(', ')}.${withVolumes ? '' : ' Volumes were kept.'}${keptNetworks}`)
        }
      }
      onStackChanged(removal.remove.completed.map((container) => container.id))
    } finally {
      setBusy('')
      closeRemoval()
    }
  }

  return (
    <section className={`containerComposeGroup ${open ? 'open' : ''}`.trim()} aria-label={`${project} Compose application`}>
      <div className="containerComposeHeader">
        <button type="button" className="containerComposeToggle" aria-expanded={open} aria-controls={bodyID} onClick={() => setOpen((value) => !value)}>
          <span className="containerComposeIdentity">
            <StatusLamp state={groupState} />
            <span className="visuallyHidden">{groupState} application state</span>
            <span>
              <strong>{project}</strong>
              <small>Compose application</small>
            </span>
          </span>
          <span className="containerComposeMeta">{services} service(s) · {containers.length} container(s) · {running} running</span>
        </button>
        <div className="containerComposeActions" role="group" aria-label={`${project} stack actions`}>
          <span className="containerComposeStatus" role="status">{busy}</span>
          {(['start', 'stop', 'restart'] as const).map((action) => (
            <ActionButton
              key={action}
              label={STACK_ACTION_TEXT[action].label}
              icon={action === 'start' ? 'play' : action}
              disabled={Boolean(busy) || (action === 'start' ? !startable : activeCount === 0)}
              onClick={() => void runStackAction(action)}
            />
          ))}
          <ActionButton
            ref={removeTrigger}
            className="removeButton"
            label="Remove stack"
            icon="remove"
            aria-expanded={confirmingRemoval}
            aria-controls={confirmingRemoval ? removalPanelID : undefined}
            disabled={Boolean(busy) || stackContainers.length === 0}
            onClick={() => confirmingRemoval ? closeRemoval() : openRemoval()}
          />
        </div>
      </div>
      {open && (
        <div className="containerComposeBody" id={bodyID}>
          {confirmingRemoval && (
            <section
              className="drawerPanel containerComposeRemoval"
              id={removalPanelID}
              aria-labelledby={removalTitleID}
              ref={removalPanel}
              tabIndex={-1}
              onKeyDown={(event) => {
                if (event.key === 'Escape' && !busy) closeRemoval()
              }}
            >
              <h3 id={removalTitleID}>Remove Compose stack {project}</h3>
              <p>
                {activeCount > 0 ? `Stops ${activeCount} running container(s), then removes ` : 'Removes '}
                all {stackContainers.length} container(s) and the stack's networks, like <code>docker compose down</code>.
              </p>
              {resources === null ? (
                <p role="status">Checking the stack's networks and volumes…</p>
              ) : (
                <>
                  <dl className="runtimeGrid">
                    <div>
                      <dt>Networks</dt>
                      <dd>{resources.networkError ? `Unavailable and kept: ${resources.networkError}` : resources.networks.join(', ') || 'None found'}</dd>
                    </div>
                    <div>
                      <dt>Volumes</dt>
                      <dd>{resources.volumeError ? `Unavailable and kept: ${resources.volumeError}` : resources.volumes.join(', ') || 'No named volumes'}</dd>
                    </div>
                  </dl>
                  <label className="containerComposeVolumeChoice">
                    <input
                      type="checkbox"
                      checked={deleteVolumes}
                      disabled={Boolean(resources.volumeError) || Boolean(busy)}
                      onChange={(event) => setDeleteVolumes(event.target.checked)}
                    />
                    <span>
                      Also delete volumes, like <code>docker compose down -v</code>. Permanently deletes the named volumes listed above and the containers' anonymous volumes.
                    </span>
                  </label>
                </>
              )}
              <div className="containerCreateActions">
                <button type="button" className="destructiveAction" disabled={resources === null || Boolean(busy)} onClick={() => void removeStack()}>
                  {deleteVolumes ? 'Confirm stack and volume removal' : 'Confirm stack removal'}
                </button>
                <button type="button" disabled={Boolean(busy)} onClick={closeRemoval}>Cancel</button>
              </div>
            </section>
          )}
          <InventoryList
            items={containers}
            getKey={(container) => container.id}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(container) => lampStateFor(container.state)}
            getLampLabel={(container) => container.state}
            selectedKey={selectedID}
            onSelect={onSelect}
            ariaLabel={`${project} Compose containers`}
            emptyMessage={`No matching containers in ${project}.`}
            columns={containerColumns(true)}
            renderActions={renderActions}
          />
        </div>
      )}
    </section>
  )
}

function ContainerTerminal({ container }: { container: DockerContainer }) {
  const dockerMode = useDockerMode()
  const [mode, setMode] = useState<'application' | 'debug'>('application')
  const [shell, setShell] = useState<TerminalShell>(dockerMode === 'windows' ? 'cmd.exe' : 'sh')
  const terminalShells = dockerMode === 'windows' ? WINDOWS_TERMINAL_SHELLS : TERMINAL_SHELLS
  const running = containerState(container) === 'running'
  const debug = mode === 'debug'
  const endpoint = containerTerminalSocketURL(container, shell, debug)

  return (
    <>
      <section className="drawerPanel">
        <h3>Terminal session</h3>
        <div className="terminalModePicker" role="group" aria-label="Container terminal target">
          <button type="button" aria-pressed={!debug} onClick={() => setMode('application')}>Application shell</button>
          <button type="button" aria-pressed={debug} disabled={!running || dockerMode === 'windows'} onClick={() => setMode('debug')}>Debug toolbox</button>
        </div>
        {!debug && (
          <div className="inspectorForm inline">
            <label>
              <span>Shell</span>
              <select value={shell} onChange={(event) => setShell(event.target.value as TerminalShell)}>
                {terminalShells.map((option) => <option value={option} key={option}>{option}</option>)}
              </select>
            </label>
          </div>
        )}
        <p className="hintLine">
          {debug
            ? 'Runs a disposable netshoot toolbox sharing the target container network, process namespace, and volumes.'
            : dockerMode === 'windows'
              ? 'Executes a Windows shell supplied by the image. The Linux netshoot toolbox is not used for Windows containers.'
              : 'Executes the selected shell inside the container. Use the debug toolbox when the image has no shell.'}
        </p>
      </section>
      <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading container terminal…</div></section>}>
        <InteractiveTerminal
          key={`${container.id}:${mode}:${shell}:${container.state}`}
          endpoint={endpoint}
          title={debug ? 'Debug terminal' : 'Container terminal'}
          detail={containerDisplayName(container)}
          running={running}
          ariaLabel={`${debug ? 'Debug' : 'Interactive'} terminal for ${container.name}`}
          stoppedMessage={isPaused(container) ? 'Resume the container to open its terminal.' : 'Start the container to open its terminal.'}
        />
      </Suspense>
    </>
  )
}
