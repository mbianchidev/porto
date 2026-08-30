import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { stripTerminalNoise } from '../format'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesEvent,
  KubernetesFileContent,
  KubernetesFileListing,
  KubernetesPod,
  KubernetesPodStats,
  KubernetesStatus,
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

function podTerminalSocketURL(pod: KubernetesPod, context: string, container: string, shell: TerminalShell): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const params = new URLSearchParams({ shell })
  if (context) params.set('context', context)
  if (container) params.set('container', container)
  return `${protocol}//${window.location.host}${podPath(pod, '/terminal', '')}?${params.toString()}`
}

function LogsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [previous, setPrevious] = useState(false)
  const logs = usePolledResource<string>(
    (signal) => apiGet(podPath(pod, '/logs', context, `container=${encodeURIComponent(container)}&previous=${previous}&tail=500`), signal),
    4000,
    [pod.namespace, pod.name, container, previous],
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
          <select value={container} onChange={(event) => setContainer(event.target.value)}>
            {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </label>
        <label className="toggleRow compact">
          <span><strong>Previous instance</strong></span>
          <input type="checkbox" checked={previous} onChange={(event) => setPrevious(event.target.checked)} />
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

function ExecTerminalTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const { notifyError } = useMessages()
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [command, setCommand] = useState('')
  const [running, setRunning] = useState(false)
  const [history, setHistory] = useState<Array<{ command: string; output: string }>>([])

  async function runCommand() {
    const trimmed = command.trim()
    if (trimmed === '') return
    setRunning(true)
    try {
      const result = await apiSend<{ output: string }>(
        podPath(pod, '/exec', context),
        'POST',
        { container, command: trimmed.split(/\s+/), stdin: '' },
      )
      setHistory((current) => [...current, { command: trimmed, output: result.output }])
      setCommand('')
    } catch (err) {
      notifyError('pods', errorMessage(err, `Unable to execute "${trimmed}"`))
    } finally {
      setRunning(false)
    }
  }

  return (
    <section className="logConsole">
      <div className="consoleHeader">
        <div>
          <h3>Terminal (one-shot)</h3>
          <p>{pod.namespace}/{pod.name}</p>
        </div>
      </div>
      <div className="inspectorForm inline">
        <label>
          <span>Container</span>
          <select value={container} onChange={(event) => setContainer(event.target.value)}>
            {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </label>
      </div>
      <div className="logViewport terminalViewport" role="log" aria-live="polite">
        {history.length === 0 && <div className="logEmpty">No commands executed yet.</div>}
        {history.map((entry, index) => (
          <pre className="logRaw" key={index}>
            <span className="terminalPrompt">$ {entry.command}</span>
            {'\n'}
            {entry.output}
          </pre>
        ))}
      </div>
      <form
        className="terminalInput"
        onSubmit={(event) => { event.preventDefault(); runCommand() }}
      >
        <input
          type="text"
          value={command}
          placeholder="cat /etc/os-release"
          aria-label="Command to execute"
          disabled={running}
          onChange={(event) => setCommand(event.target.value)}
        />
        <button type="submit" disabled={running || command.trim() === ''}>{running ? 'Running…' : 'Run'}</button>
      </form>
    </section>
  )
}

type LiveTerminalState = 'connecting' | 'open' | 'ended' | 'unavailable'

const CONNECT_TIMEOUT_MS = 4000
const TERMINAL_BUFFER_LIMIT = 200_000

/**
 * Interactive pod terminal over the daemon's WebSocket bridge to `kubectl exec
 * --stdin --tty`. Falls back to the one-shot POST /exec form (ExecTerminalTab)
 * whenever the socket never reaches the open state — offline daemon, blocked
 * WebSocket upgrade, or an environment without WebSocket support.
 */
function LiveTerminalTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const [container, setContainer] = useState(pod.containers[0]?.name ?? '')
  const [shell, setShell] = useState<TerminalShell>('sh')
  const [state, setState] = useState<LiveTerminalState>(() => (typeof WebSocket === 'undefined' ? 'unavailable' : 'connecting'))
  const [output, setOutput] = useState('')
  const [inputLine, setInputLine] = useState('')
  const [sessionToken, setSessionToken] = useState(0)
  const socketRef = useRef<WebSocket | null>(null)
  const outputRef = useRef<HTMLPreElement>(null)
  const terminalURL = podTerminalSocketURL(pod, context, container, shell)

  useEffect(() => {
    if (typeof WebSocket === 'undefined') return
    let opened = false
    const decoder = new TextDecoder('utf-8')
    const socket = new WebSocket(terminalURL)
    socket.binaryType = 'arraybuffer'
    socketRef.current = socket

    const connectTimeout = window.setTimeout(() => {
      if (!opened) socket.close()
    }, CONNECT_TIMEOUT_MS)

    socket.onopen = () => {
      opened = true
      window.clearTimeout(connectTimeout)
      setState('open')
    }
    socket.onmessage = (event) => {
      let chunk = ''
      if (event.data instanceof ArrayBuffer) {
        chunk = decoder.decode(new Uint8Array(event.data), { stream: true })
      } else if (typeof event.data === 'string') {
        chunk = event.data
      }
      if (chunk === '') return
      setOutput((current) => (current + stripTerminalNoise(chunk)).slice(-TERMINAL_BUFFER_LIMIT))
    }
    socket.onclose = () => {
      window.clearTimeout(connectTimeout)
      setState(opened ? 'ended' : 'unavailable')
    }

    return () => {
      window.clearTimeout(connectTimeout)
      socket.close()
      socketRef.current = null
    }
  }, [terminalURL, sessionToken])

  useEffect(() => {
    const node = outputRef.current
    if (node) node.scrollTop = node.scrollHeight
  }, [output])

  function sendInput(text: string) {
    const socket = socketRef.current
    if (!socket || socket.readyState !== WebSocket.OPEN) return
    socket.send(text)
  }

  if (state === 'unavailable') {
    return (
      <>
        <p className="hintLine">Live terminal unavailable; falling back to one-shot command execution.</p>
        <ExecTerminalTab pod={pod} context={context} />
      </>
    )
  }

  const connected = state === 'open'
  return (
    <section className="logConsole">
      <div className="consoleHeader">
        <div>
          <h3>Terminal</h3>
          <p>
            {pod.namespace}/{pod.name}
            {' · '}
            {state === 'connecting' ? 'connecting…' : connected ? 'connected' : 'session ended'}
          </p>
        </div>
        <div className="consoleActions">
          {state === 'ended' && (
            <button type="button" onClick={() => {
              setState('connecting')
              setOutput('')
              setSessionToken((value) => value + 1)
            }}>Reconnect</button>
          )}
        </div>
      </div>
      <div className="inspectorForm inline">
        <label>
          <span>Container</span>
          <select value={container} disabled={connected} onChange={(event) => {
            setState('connecting')
            setOutput('')
            setContainer(event.target.value)
          }}>
            {pod.containers.map((item) => <option key={item.name} value={item.name}>{item.name}</option>)}
          </select>
        </label>
        <label>
          <span>Shell</span>
          <select value={shell} disabled={connected} onChange={(event) => {
            setState('connecting')
            setOutput('')
            setShell(event.target.value as TerminalShell)
          }}>
            {TERMINAL_SHELLS.map((option) => <option key={option} value={option}>{option}</option>)}
          </select>
        </label>
        <ActionButton label="Send Ctrl+C" icon="stop" disabled={!connected} onClick={() => sendInput('\x03')} />
        <ActionButton label="Send Ctrl+D" icon="close" disabled={!connected} onClick={() => sendInput('\x04')} />
      </div>
      <pre className="logViewport terminalViewport logRaw" ref={outputRef} role="log" aria-live="polite">
        {output || (state === 'connecting' ? 'Connecting…' : '')}
      </pre>
      <form
        className="terminalInput"
        onSubmit={(event) => {
          event.preventDefault()
          if (inputLine === '') return
          sendInput(`${inputLine}\n`)
          setInputLine('')
        }}
      >
        <input
          type="text"
          value={inputLine}
          placeholder="type a command, press Enter"
          aria-label="Terminal input"
          disabled={!connected}
          onChange={(event) => setInputLine(event.target.value)}
        />
        <button type="submit" disabled={!connected}>Send</button>
      </form>
    </section>
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

  const listing = usePolledResource<KubernetesFileListing>(
    (signal) => apiGet(podPath(pod, '/files', context, `container=${encodeURIComponent(container)}&path=${encodeURIComponent(browsePath)}`), signal),
    0,
    [pod.namespace, pod.name, container, browsePath],
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
          <select value={container} onChange={(event) => setContainer(event.target.value)}>
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
      {listing.error && <p className="errorLine">{listing.error}</p>}
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
      {openFile && (
        <div className="fileEditor">
          <div className="commandStrip"><span>Editing</span><code>{openFile.path}{openFile.truncated ? ' (truncated)' : ''}</code></div>
          <textarea value={draft} onChange={(event) => setDraft(event.target.value)} rows={12} spellCheck={false} />
          <div className="actions">
            <button type="button" onClick={saveFile} disabled={saving}>{saving ? 'Saving…' : 'Save file'}</button>
            <button className="destructiveAction" type="button" onClick={deleteFile}>Delete file</button>
            <button type="button" onClick={() => setOpenFile(null)}>Close</button>
          </div>
        </div>
      )}
    </section>
  )
}

function StatsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const stats = usePolledResource<KubernetesPodStats[]>(
    (signal) => apiGet(podPath(pod, '/stats', context), signal),
    5000,
    [pod.namespace, pod.name],
  )
  return (
    <section className="drawerPanel">
      <h3>Container stats</h3>
      {stats.error && <p className="errorLine">{stats.error}</p>}
      <dl className="runtimeGrid">
        {(stats.data ?? []).map((item) => (
          <div key={item.container}><dt>{item.container}</dt><dd>CPU {item.cpu} · Memory {item.memory}</dd></div>
        ))}
      </dl>
      {stats.data && stats.data.length === 0 && <p>No metrics reported (metrics-server may be unavailable).</p>}
    </section>
  )
}

function EventsTab({ pod, context }: { pod: KubernetesPod; context: string }) {
  const events = usePolledResource<KubernetesEvent[]>(
    (signal) => apiGet(podPath(pod, '/events', context), signal),
    8000,
    [pod.namespace, pod.name],
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
  const manifest = usePolledResource<string>(
    (signal) => apiGet(podPath(pod, '/manifest', context), signal),
    0,
    [pod.namespace, pod.name],
  )
  return (
    <section className="drawerPanel">
      <h3>Manifest</h3>
      {manifest.error && <p className="errorLine">{manifest.error}</p>}
      {manifest.data && <pre className="logRaw">{manifest.data}</pre>}
    </section>
  )
}

export function Pods({ context }: { context: string }) {
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const [tab, setTab] = useState('overview')

  const status = usePolledResource<KubernetesStatus>(
    (signal) => apiGet(`/api/kubernetes/status?context=${encodeURIComponent(context)}`, signal),
    10000,
    [context],
  )
  const pods = usePolledResource<KubernetesPod[]>(
    (signal) => apiGet(`/api/kubernetes/pods?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal),
    5000,
    [context, namespace],
  )
  const items = pods.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((pod) => normalizedQuery === '' || [pod.name, pod.namespace, pod.node]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const key = (pod: KubernetesPod) => `${pod.namespace}/${pod.name}`
  const selected = items.find((pod) => key(pod) === selectedKey) ?? null
  const available = status.data?.available ?? false

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
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} pods</span>
        <button className="refreshControl" type="button" onClick={pods.reload}>Refresh</button>
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
            {tab === 'terminal' && <LiveTerminalTab key={`${selected.namespace}/${selected.name}`} pod={selected} context={context} />}
            {tab === 'files' && <FilesTab pod={selected} context={context} />}
            {tab === 'stats' && <StatsTab pod={selected} context={context} />}
            {tab === 'events' && <EventsTab pod={selected} context={context} />}
            {tab === 'manifest' && <ManifestTab pod={selected} context={context} />}
          </Inspector>
        )}
      </div>
    </>
  )
}
