import { lazy, Suspense, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { writeClipboard } from '../clipboard'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorErrorBoundary, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesEvent,
  KubernetesContainerCapabilities,
  KubernetesContext,
  KubernetesFileContent,
  KubernetesFileListing,
  KubernetesPod,
  KubernetesPodStats,
} from '../types'

const COLUMNS_TEMPLATE = '12px minmax(170px,1.3fr) minmax(110px,0.7fr) minmax(80px,0.4fr) minmax(70px,0.4fr) minmax(120px,0.8fr) minmax(70px,0.4fr)'
const TABS = [
  { id: 'overview', label: 'Overview' },
  { id: 'logs', label: 'Logs' },
  { id: 'terminal', label: 'Terminal' },
  { id: 'files', label: 'Files' },
  { id: 'stats', label: 'Stats' },
  { id: 'events', label: 'Events' },
  { id: 'manifest', label: 'Manifest' },
]

function podPath(pod: KubernetesPod, suffix: string, context: string, extra = ''): string {
  const contextParam = context ? `context=${encodeURIComponent(context)}` : ''
  const join = extra || contextParam ? '?' : ''
  const params = [contextParam, extra].filter(Boolean).join('&')
  return `/api/kubernetes/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}${suffix}${join}${params}`
}

const TERMINAL_SHELLS = ['sh', 'bash', 'ash'] as const
type TerminalShell = (typeof TERMINAL_SHELLS)[number]
const InteractiveTerminal = lazy(async () => {
  const terminal = await import('../components/VMTerminal')
  return { default: terminal.InteractiveTerminal }
})

function podTerminalSocketURL(pod: KubernetesPod, context: string, container: string, shell: TerminalShell): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const params = new URLSearchParams({ shell })
  if (context) params.set('context', context)
  if (container) params.set('container', container)
  return `${protocol}//${window.location.host}${podPath(pod, '/terminal', '')}?${params.toString()}`
}

function isTerminalShell(value: string): value is TerminalShell {
  return TERMINAL_SHELLS.includes(value as TerminalShell)
}

function LogsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [previous, setPrevious] = useState(false)
  const selectedContainer = pod.containers.find((item) => item.name === container)
  const previousAvailable = (selectedContainer?.restartCount ?? 0) > 0
  const showPrevious = previous && previousAvailable
  const logs = usePolledResource<string>(
    (signal) => apiGet(podPath(pod, '/logs', context, `container=${encodeURIComponent(container)}&previous=${showPrevious}&tail=500`), signal),
    4000,
    [context, pod.namespace, pod.name, container, showPrevious],
    `kubernetes:${context}:pod:${pod.uid}:logs:${container}:${showPrevious}`,
  )
  return (
    <section className="logConsole">
      <div className="consoleHeader">
        <div>
          <h3>Container logs</h3>
          <p>{pod.namespace}/{pod.name}</p>
        </div>
        <div className="consoleActions">
          <button type="button" onClick={logs.reload}>Refresh</button>
        </div>
      </div>
      <div className="inspectorForm inline">
        <label>
          <span>Container</span>
          <select
            value={container}
            onChange={(event) => {
              setContainer(event.target.value)
              setPrevious(false)
            }}
          >
            {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </label>
        <label className="toggleRow compact">
          <span>
            <strong>Previous instance</strong>
            {!previousAvailable && <small>No terminated instance available.</small>}
          </span>
          <input
            type="checkbox"
            checked={showPrevious}
            disabled={!previousAvailable}
            onChange={(event) => setPrevious(event.target.checked)}
          />
        </label>
      </div>
      <div className="logViewport" role="log" aria-live="polite" aria-busy={logs.loading}>
        {logs.error && <div className="logEmpty errorLine">{logs.error}</div>}
        {!logs.error && logs.loading && !logs.data && <div className="logEmpty">Loading container output…</div>}
        {!logs.error && logs.data === '' && <div className="logEmpty">No output captured yet.</div>}
        {!logs.error && logs.data && (
          <pre className="logRaw">{logs.data}</pre>
        )}
      </div>
    </section>
  )
}

function PodTerminalTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [shell, setShell] = useState<TerminalShell>('sh')
  const running = pod.phase.toLocaleLowerCase() === 'running'
  const capabilities = usePolledResource<KubernetesContainerCapabilities>(
    (signal) => running
      ? apiGet(podPath(pod, '/capabilities', context, `container=${encodeURIComponent(container)}`), signal)
      : Promise.resolve({
        shells: [],
        fileInspection: false,
        message: 'The pod must be running before Porto can inspect its terminal capabilities.',
      }),
    0,
    [context, pod.namespace, pod.name, container, running],
    `kubernetes:${context}:pod:${pod.uid}:capabilities:${container}`,
  )
  const availableShells = (capabilities.data?.shells ?? []).filter(isTerminalShell)
  const activeShell = availableShells.includes(shell) ? shell : availableShells[0] ?? 'sh'
  const ready = !capabilities.loading && !capabilities.error && availableShells.length > 0
  const terminalURL = podTerminalSocketURL(pod, context, container, activeShell)
  return (
    <>
      <section className="drawerPanel">
        <h3>Terminal session</h3>
        <div className="inspectorForm inline">
          <label>
            <span>Container</span>
            <select value={container} onChange={(event) => setContainer(event.target.value)}>
              {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
            </select>
          </label>
          <label>
            <span>Shell</span>
            <select
              value={ready ? activeShell : ''}
              disabled={!ready}
              onChange={(event) => setShell(event.target.value as TerminalShell)}
            >
              {!ready && <option value="">No supported shell</option>}
              {availableShells.map((option) => <option key={option} value={option}>{option}</option>)}
            </select>
          </label>
        </div>
        {capabilities.loading && <p className="hintLine">Inspecting container capabilities...</p>}
        {capabilities.error && <p className="errorLine">{capabilities.error}</p>}
        {!capabilities.loading && !capabilities.error && capabilities.data?.message && (
          <p className="hintLine">{capabilities.data.message}</p>
        )}
      </section>
      {(ready || !running) && (
        <Suspense fallback={<div className="terminalPlaceholder">Loading terminal...</div>}>
          <InteractiveTerminal
            key={terminalURL}
            endpoint={terminalURL}
            title="Pod terminal"
            detail={`${pod.namespace}/${pod.name} · ${container || 'default container'} · ${activeShell}`}
            running={running}
            ariaLabel={`Interactive terminal for ${pod.namespace}/${pod.name}`}
            stoppedMessage="The pod must be running to open a terminal."
          />
        </Suspense>
      )}
    </>
  )
}

function FilesTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [path, setPath] = useState('/')
  const [browsePath, setBrowsePath] = useState('/')
  const [openFile, setOpenFile] = useState<KubernetesFileContent | null>(null)
  const [draft, setDraft] = useState('')
  const [saving, setSaving] = useState(false)
  const running = pod.phase.toLocaleLowerCase() === 'running'

  const capabilities = usePolledResource<KubernetesContainerCapabilities>(
    (signal) => running
      ? apiGet(podPath(pod, '/capabilities', context, `container=${encodeURIComponent(container)}&files=true`), signal)
      : Promise.resolve({
        shells: [],
        fileInspection: false,
        message: 'The pod must be running before Porto can inspect its files.',
      }),
    0,
    [context, pod.namespace, pod.name, container, running],
    `kubernetes:${context}:pod:${pod.uid}:file-capabilities:${container}`,
  )
  const canInspect = !capabilities.loading && !capabilities.error && (capabilities.data?.fileInspection ?? false)
  const listing = usePolledResource<KubernetesFileListing>(
    (signal) => canInspect
      ? apiGet(podPath(pod, '/files', context, `container=${encodeURIComponent(container)}&path=${encodeURIComponent(browsePath)}`), signal)
      : Promise.resolve({ path: browsePath, entries: [] }),
    0,
    [context, pod.namespace, pod.name, container, browsePath, canInspect],
    `kubernetes:${context}:pod:${pod.uid}:files:${container}:${browsePath}`,
  )

  async function openEntry(entryName: string, entryType: string) {
    const nextPath = browsePath.endsWith('/') ? `${browsePath}${entryName}` : `${browsePath}/${entryName}`
    if (entryType === 'directory' || entryType === 'dir') {
      setBrowsePath(nextPath)
      return
    }
    try {
      const content = await apiGet<KubernetesFileContent>(
        podPath(pod, '/file', context, `container=${encodeURIComponent(container)}&path=${encodeURIComponent(nextPath)}`),
      )
      setOpenFile(content)
      setDraft(content.content)
    } catch (err) {
      notifyError('pods', errorMessage(err, `Unable to read ${nextPath}`))
    }
  }

  async function saveFile() {
    if (!openFile) return
    setSaving(true)
    try {
      await apiSend(podPath(pod, '/file', context), 'PUT', { container, path: openFile.path, content: draft })
      notifyNotice('pods', `Saved ${openFile.path}.`)
    } catch (err) {
      notifyError('pods', errorMessage(err, `Unable to save ${openFile.path}`))
    } finally {
      setSaving(false)
    }
  }

  async function deleteFile() {
    if (!openFile) return
    if (!window.confirm(`Delete ${openFile.path} from ${pod.name}?`)) return
    try {
      await apiSend(podPath(pod, '/file', context, `container=${encodeURIComponent(container)}&path=${encodeURIComponent(openFile.path)}&confirm=true`), 'DELETE')
      notifyNotice('pods', `Deleted ${openFile.path}.`)
      setOpenFile(null)
      listing.reload()
    } catch (err) {
      notifyError('pods', errorMessage(err, `Unable to delete ${openFile.path}`))
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Files</h3>
      <div className="inspectorForm inline">
        <label>
          <span>Container</span>
          <select value={container} onChange={(event) => {
            setContainer(event.target.value)
            setOpenFile(null)
          }}>
            {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </label>
        <form onSubmit={(event) => { event.preventDefault(); setBrowsePath(path) }}>
          <label>
            <span>Path</span>
            <input type="text" value={path} onChange={(event) => setPath(event.target.value)} />
          </label>
          <button type="submit">Browse</button>
        </form>
      </div>
      {capabilities.loading && <p className="hintLine">Inspecting container capabilities...</p>}
      {capabilities.error && <p className="errorLine">{capabilities.error}</p>}
      {!capabilities.loading && !capabilities.error && !canInspect && (
        <p className="hintLine">{capabilities.data?.message || 'File inspection is unavailable for this container.'}</p>
      )}
      {canInspect && listing.error && <p className="errorLine">{listing.error}</p>}
      {canInspect && (
        <ul className="fileList">
          {(listing.data?.entries ?? []).map((entry) => (
            <li key={entry.name}>
              <button type="button" onClick={() => openEntry(entry.name, entry.type)}>
                <span className="fileKind">{entry.type === 'directory' || entry.type === 'dir' ? 'DIR' : 'FILE'}</span>
                {entry.name}
                <small>{entry.type === 'file' ? `${entry.size}B` : ''}</small>
              </button>
            </li>
          ))}
          {listing.data && listing.data.entries.length === 0 && <li className="logEmpty">Empty directory.</li>}
        </ul>
      )}
      {canInspect && openFile && (
        <div className="fileEditor">
          <div className="commandStrip"><span>Editing</span><code>{openFile.path}{openFile.truncated ? ' (truncated)' : ''}</code></div>
          {openFile.truncated && (
            <p className="errorLine">This file exceeds the safe preview limit. Editing is disabled to prevent truncating its contents.</p>
          )}
          <textarea
            value={draft}
            readOnly={openFile.truncated}
            onChange={(event) => setDraft(event.target.value)}
            rows={12}
            spellCheck={false}
          />
          <div className="actions">
            <button type="button" onClick={saveFile} disabled={saving || openFile.truncated}>{saving ? 'Saving…' : 'Save file'}</button>
            <button className="destructiveAction" type="button" onClick={deleteFile}>Delete file</button>
            <button type="button" onClick={() => setOpenFile(null)}>Close</button>
          </div>
        </div>
      )}
    </section>
  )
}

function StatsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const ready = pod.phase.toLocaleLowerCase() === 'running'
    && pod.podReady
  const stats = usePolledResource<KubernetesPodStats[]>(
    (signal) => ready ? apiGet(podPath(pod, '/stats', context), signal) : Promise.resolve([]),
    5000,
    [context, pod.namespace, pod.name, ready],
    ready ? `kubernetes:${context}:pod:${pod.uid}:stats` : undefined,
  )
  return (
    <section className="drawerPanel">
      <h3>Container stats</h3>
      {!ready ? (
        <p>Metrics are available only while the pod is Running and Ready.</p>
      ) : (
        <>
          {stats.error && <p className="errorLine">{stats.error}</p>}
          <dl className="runtimeGrid">
            {(stats.data ?? []).map((item) => (
              <div key={item.container}><dt>{item.container}</dt><dd>CPU {item.cpu} · Memory {item.memory}</dd></div>
            ))}
          </dl>
          {stats.data && stats.data.length === 0 && <p>No metrics reported yet.</p>}
        </>
      )}
    </section>
  )
}

function EventsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const events = usePolledResource<KubernetesEvent[]>(
    (signal) => apiGet(podPath(pod, '/events', context), signal),
    8000,
    [context, pod.namespace, pod.name],
    `kubernetes:${context}:pod:${pod.uid}:events`,
  )
  return (
    <section className="drawerPanel">
      <h3>Events</h3>
      {events.error && <p className="errorLine">{events.error}</p>}
      <div className="logViewport">
        {(events.data ?? []).map((event, index) => (
          <div className={`logLine ${event.type === 'Warning' ? 'stderr' : 'stdout'}`} key={index}>
            <time>{event.lastSeen}</time>
            <span className="streamLabel">{event.reason}</span>
            <span className="logMessage">{event.message} ({event.count}×)</span>
          </div>
        ))}
        {events.data && events.data.length === 0 && <div className="logEmpty">No recent events.</div>}
      </div>
    </section>
  )
}

function ManifestTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const manifest = usePolledResource<string>(
    (signal) => apiGet(podPath(pod, '/manifest', context), signal),
    0,
    [context, pod.namespace, pod.name],
    `kubernetes:${context}:pod:${pod.uid}:manifest`,
  )
  async function copyManifest() {
    if (!manifest.data) return
    try {
      await writeClipboard(manifest.data)
      notifyNotice('pods', `Copied the ${pod.name} manifest.`)
    } catch (err) {
      notifyError('pods', errorMessage(err, 'Unable to copy the manifest'))
    }
  }
  return (
    <section className="drawerPanel">
      <div className="drawerPanelHeading">
        <h3>Manifest</h3>
        <ActionButton label="Copy manifest" icon="copy" disabled={!manifest.data} onClick={copyManifest} />
      </div>
      {manifest.error && <p className="errorLine">{manifest.error}</p>}
      {manifest.data && <pre className="logViewport manifestViewport logRaw">{manifest.data}</pre>}
    </section>
  )
}

export function Pods({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const [tab, setTab] = useState('overview')
  const [inspectorGeneration, setInspectorGeneration] = useState(0)

  const status = useKubernetesStatus(context)
  const available = status.data?.available ?? false
  const pods = usePolledResource<KubernetesPod[]>(
    (signal) => available
      ? apiGet(`/api/kubernetes/pods?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    5000,
    [context, namespace, available],
    available ? `kubernetes:${context}:pods:${namespace}` : undefined,
  )
  const items = pods.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((pod) => normalizedQuery === '' || [pod.name, pod.namespace, pod.node]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const key = (pod: KubernetesPod) => `${pod.namespace}/${pod.name}`
  const selected = items.find((pod) => key(pod) === selectedKey) ?? null

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Pod signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} pod(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter pods</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter pods by name or node" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <label className="namespaceField">
          <span>Namespace</span>
          <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} pods</span>
        <button className="refreshControl" type="button" onClick={() => {
          setInspectorGeneration((generation) => generation + 1)
          status.reload()
          pods.reload()
        }}>Refresh</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Kubernetes" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={key}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(pod) => lampStateFor(pod.phase)}
            getLampLabel={(pod) => pod.phase}
            selectedKey={selectedKey}
            onSelect={(pod) => { setSelectedKey(key(pod)); setTab('overview') }}
            ariaLabel="Kubernetes pods"
            emptyMessage={pods.error || 'No pods found in this namespace.'}
            columns={[
              { header: 'Name', render: (pod) => <strong>{pod.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (pod) => pod.namespace },
              { header: 'Ready', className: 'mono', render: (pod) => pod.ready },
              { header: 'Restarts', className: 'mono', render: (pod) => pod.restarts },
              { header: 'Node', className: 'mono', render: (pod) => pod.node || '—' },
              { header: 'Age', className: 'mono', render: (pod) => pod.age },
            ]}
          />
        )}

        {selected && (
          <Inspector title={selected.name} subtitle={`${selected.namespace} · ${selected.phase}`} onClose={() => setSelectedKey(null)}>
            <InspectorTabs tabs={TABS} activeID={tab} onSelect={setTab} />
            <InspectorErrorBoundary key={`${context}:${selectedKey}:${tab}:${inspectorGeneration}`}>
              {tab === 'overview' && (
                <section className="drawerPanel">
                  <h3>Pod overview</h3>
                  <dl className="runtimeGrid">
                    <div><dt>Phase</dt><dd>{selected.phase}</dd></div>
                    <div><dt>Ready</dt><dd>{selected.ready}</dd></div>
                    <div><dt>Node</dt><dd>{selected.node || '—'}</dd></div>
                    <div><dt>Pod IP</dt><dd>{selected.ip || '—'}</dd></div>
                    <div><dt>Age</dt><dd>{selected.age}</dd></div>
                    <div><dt>Restarts</dt><dd>{selected.restarts}</dd></div>
                  </dl>
                  <h3>Containers</h3>
                  <dl className="runtimeGrid">
                    {selected.containers.map((container) => (
                      <div key={container.name}>
                        <dt>{container.name}</dt>
                        <dd>{container.image} · {container.state}{container.ready ? ' · ready' : ' · not ready'} · {container.restartCount} restart(s)</dd>
                      </div>
                    ))}
                  </dl>
                </section>
              )}
              {tab === 'logs' && <LogsTab pod={selected} context={context} />}
              {tab === 'terminal' && <PodTerminalTab key={`${selected.namespace}/${selected.name}`} pod={selected} context={context} />}
              {tab === 'files' && <FilesTab pod={selected} context={context} />}
              {tab === 'stats' && <StatsTab pod={selected} context={context} />}
              {tab === 'events' && <EventsTab pod={selected} context={context} />}
              {tab === 'manifest' && <ManifestTab pod={selected} context={context} />}
            </InspectorErrorBoundary>
          </Inspector>
        )}
      </div>
    </>
  )
}
