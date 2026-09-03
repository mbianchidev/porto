import { lazy, Suspense, useState, type FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { useContainerSnapshots } from '../containerSnapshots'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  DockerContainer,
  DockerContainerAction,
  DockerContainerCreateRequest,
  DockerContainerCreateResult,
  DockerImage,
  DockerStatus,
} from '../types'

const COLUMNS_TEMPLATE = '12px minmax(180px,1.3fr) minmax(160px,1fr) minmax(150px,1fr) minmax(120px,0.8fr)'
const EMPTY_CREATE_DRAFT = {
  name: '',
  image: '',
  hostPort: '',
  containerPort: '',
  healthCommand: '',
}
const InteractiveTerminal = lazy(async () => {
  const terminal = await import('../components/VMTerminal')
  return { default: terminal.InteractiveTerminal }
})
const TERMINAL_SHELLS = ['sh', 'bash', 'ash', '/bin/bash'] as const
type TerminalShell = (typeof TERMINAL_SHELLS)[number]

function containerState(container: DockerContainer) {
  return container.state.toLocaleLowerCase()
}

function isPaused(container: DockerContainer) {
  return containerState(container) === 'paused'
}

function isStopped(container: DockerContainer) {
  return ['stopped', 'exited'].includes(containerState(container))
}

function isStartable(container: DockerContainer) {
  return ['stopped', 'exited', 'created'].includes(containerState(container))
}

function isActive(container: DockerContainer) {
  return ['running', 'paused', 'pausing', 'restarting'].includes(containerState(container))
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

export function Containers() {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [creating, setCreating] = useState(false)
  const [createDraft, setCreateDraft] = useState(EMPTY_CREATE_DRAFT)
  const [containerTab, setContainerTab] = useState('overview')

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const containers = useContainerSnapshots(status.data?.enabled ?? false)
  const items = containers.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((container) => normalizedQuery === '' || [container.name, container.image, container.status]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const selected = items.find((container) => container.id === selectedID) ?? null
  const available = containers.snapshot?.available ?? status.data?.available ?? false
  const stale = containers.snapshot?.stale ?? status.data?.stale ?? false
  const images = usePolledResource<DockerImage[]>(
    (signal) => available ? apiGet('/api/docker/images', signal) : Promise.resolve([]),
    15000,
    [available],
    available ? 'docker:images:create' : undefined,
  )
  const imageOptions = [...new Set((images.data ?? [])
    .filter((image) => image.repository && image.repository !== '<none>')
    .map((image) => image.tag && image.tag !== '<none>' ? `${image.repository}:${image.tag}` : image.repository))]

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

  function containerActions(container: DockerContainer) {
    const paused = isPaused(container)
    const stopped = isStopped(container)
    const active = isActive(container)
    return (
      <>
        <ActionButton label="Start container" icon="play" disabled={!isStartable(container)} onClick={() => containerAction(container, 'start')} />
        <ActionButton label="Resume container" icon="play" disabled={!paused} onClick={() => containerAction(container, 'unpause')} />
        <ActionButton label="Stop container" icon="stop" disabled={!active} onClick={() => containerAction(container, 'stop')} />
        <ActionButton label="Restart container" icon="restart" disabled={!active} onClick={() => containerAction(container, 'restart')} />
        <ActionButton
          className="removeButton"
          label={stopped ? 'Remove container' : 'Remove container (stop it first)'}
          icon="remove"
          disabled={!stopped}
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
          <StatusLamp state={available ? 'running' : stale ? 'starting' : 'crashed'} />
          {available ? 'Live' : stale ? 'Stale' : 'Unavailable'}
        </span>
        {status.data?.context && <span className="fleetDatum"><small>Context</small><strong>{status.data.context}</strong></span>}
        {containers.snapshot?.namespace && <span className="fleetDatum"><small>Namespace</small><strong>{containers.snapshot.namespace}</strong></span>}
        <span className="fleetMessage">
          {items.length} container(s) · revision {containers.snapshot?.revision ?? 0}
          {!containers.connected && available ? ' · reconnecting updates' : ''}
        </span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter containers</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter containers by name, image, or status" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} containers</span>
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
            message={containers.snapshot?.message || containers.error || status.data?.message || status.error || (status.data?.enabled ? 'Confirm the configured Docker-compatible engine is running.' : undefined)}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(container) => container.id}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(container) => lampStateFor(container.state)}
            getLampLabel={(container) => container.state}
            selectedKey={selectedID}
            onSelect={(container) => { setSelectedID(container.id); setContainerTab('overview') }}
            ariaLabel="Docker containers"
            emptyMessage={containers.error || 'No containers found.'}
            columns={[
              { header: 'Name', render: (container) => <strong>{container.name.replace(/^\//, '')}</strong> },
              { header: 'Image', className: 'mono', render: (container) => container.image },
              { header: 'Status', className: 'mono', render: (container) => container.status },
              { header: 'Ports', className: 'mono', render: (container) => container.ports || '—' },
            ]}
            renderActions={containerActions}
          />
        )}

        {selected && (
          <Inspector title={selected.name.replace(/^\//, '')} subtitle={selected.image} onClose={() => setSelectedID(null)}>
            <InspectorTabs
              tabs={[
                { id: 'overview', label: 'Overview' },
                { id: 'terminal', label: 'Terminal' },
              ]}
              activeID={containerTab}
              onSelect={setContainerTab}
            />
            {containerTab === 'overview' && (
              <>
                <div className="drawerReadouts" aria-label="Container readouts">
                  <span><small>State</small><strong>{selected.state}</strong></span>
                  <span><small>Task</small><strong>{taskLabel(selected)}</strong></span>
                  <span><small>Health</small><strong>{healthLabel(selected.health.status)}</strong></span>
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
                    <div><dt>CPU quota</dt><dd>{selected.resources.cpuQuota ?? '—'}</dd></div>
                    <div><dt>Memory limit</dt><dd>{selected.resources.memoryLimit ?? '—'}</dd></div>
                    <div>
                      <dt>Stop behavior</dt>
                      <dd>
                        {selected.stopSignal || 'default signal'} / {selected.stopTimeout === undefined ? 'default timeout' : `${selected.stopTimeout}s`}
                      </dd>
                    </div>
                  </dl>
                  {selected.inventoryError && <p className="errorText">{selected.inventoryError}</p>}
                </section>
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
          </Inspector>
        )}
      </div>
    </>
  )
}

function ContainerTerminal({ container }: { container: DockerContainer }) {
  const [mode, setMode] = useState<'application' | 'debug'>('application')
  const [shell, setShell] = useState<TerminalShell>('sh')
  const running = containerState(container) === 'running'
  const debug = mode === 'debug'
  const endpoint = containerTerminalSocketURL(container, shell, debug)

  return (
    <>
      <section className="drawerPanel">
        <h3>Terminal session</h3>
        <div className="terminalModePicker" role="group" aria-label="Container terminal target">
          <button type="button" aria-pressed={!debug} onClick={() => setMode('application')}>Application shell</button>
          <button type="button" aria-pressed={debug} disabled={!running} onClick={() => setMode('debug')}>Debug toolbox</button>
        </div>
        {!debug && (
          <div className="inspectorForm inline">
            <label>
              <span>Shell</span>
              <select value={shell} onChange={(event) => setShell(event.target.value as TerminalShell)}>
                {TERMINAL_SHELLS.map((option) => <option value={option} key={option}>{option}</option>)}
              </select>
            </label>
          </div>
        )}
        <p className="hintLine">
          {debug
            ? 'Runs a disposable netshoot toolbox sharing the target container network, process, IPC, UTS, and volumes.'
            : 'Executes the selected shell inside the container. Use the debug toolbox when the image has no shell.'}
        </p>
      </section>
      <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading container terminal…</div></section>}>
        <InteractiveTerminal
          key={`${container.id}:${mode}:${shell}:${container.state}`}
          endpoint={endpoint}
          title={debug ? 'Debug terminal' : 'Container terminal'}
          detail={container.name.replace(/^\//, '')}
          running={running}
          ariaLabel={`${debug ? 'Debug' : 'Interactive'} terminal for ${container.name}`}
          stoppedMessage={isPaused(container) ? 'Resume the container to open its terminal.' : 'Start the container to open its terminal.'}
        />
      </Suspense>
    </>
  )
}
