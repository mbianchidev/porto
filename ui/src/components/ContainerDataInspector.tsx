import { useEffect, useRef, useState } from 'react'
import { apiGet, errorMessage } from '../api'
import { writeClipboard } from '../clipboard'
import { isLogRecord, LogBuffer, type BufferedLog } from '../containerLogs'
import { bytesLabel, type InspectorStats, type JSONValue } from '../dataTypes'
import { stripTerminalNoise } from '../format'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'

export function ContainerLogs({ id }: { id: string }) {
  const { notifyError } = useMessages()
  const [follow, setFollow] = useState(true)
  const [generation, setGeneration] = useState(0)
  const [query, setQuery] = useState('')
  const [stream, setStream] = useState('all')
  const [records, setRecords] = useState<BufferedLog[]>([])
  const [dropped, setDropped] = useState(0)
  const [message, setMessage] = useState('')
  const [connection, setConnection] = useState('Connecting')
  const buffer = useRef(new LogBuffer())
  useEffect(() => {
    if (!follow) return
    const current = new LogBuffer()
    buffer.current = current
    let frame = 0
    const source = new EventSource(`/api/docker/containers/${encodeURIComponent(id)}/logs/stream?tail=500`)
    source.onopen = () => setConnection('Live')
    source.addEventListener('capabilities', (event) => {
      try {
        const value: unknown = JSON.parse(event.data)
        if (value && typeof value === 'object' && 'message' in value && typeof value.message === 'string') setMessage(value.message)
      } catch (err) { setMessage(errorMessage(err, 'Invalid log capability response')); source.close() }
    })
    source.addEventListener('log', (event) => {
      try {
        const value: unknown = JSON.parse(event.data)
        if (!isLogRecord(value)) throw new Error('Invalid bounded log record')
        current.push({ ...value, text: stripTerminalNoise(value.text) })
        if (!frame) frame = requestAnimationFrame(() => {
          frame = 0
          setRecords(current.snapshot())
          setDropped(current.dropped)
        })
      } catch (err) { setMessage(errorMessage(err, 'Invalid log record')); source.close(); setConnection('Disconnected') }
    })
    source.addEventListener('failure', (event) => {
      try {
        const value: unknown = JSON.parse(event.data)
        setMessage(value && typeof value === 'object' && 'message' in value && typeof value.message === 'string' ? value.message : 'Log stream failed')
      } catch (err) { setMessage(errorMessage(err, 'Log stream failed')) }
      source.close()
      setConnection('Disconnected')
    })
    source.onerror = () => { source.close(); setConnection('Disconnected'); setMessage('Log connection lost. Reconnect to reload retained history.') }
    return () => { source.close(); if (frame) cancelAnimationFrame(frame) }
  }, [id, follow, generation])
  const search = query.toLocaleLowerCase()
  const filtered = records.filter((record) => (stream === 'all' || record.stream === stream) && record.text.toLocaleLowerCase().includes(search))
  async function copy() {
    try { await writeClipboard(filtered.map((record) => `${record.timestamp ?? 'untimed'} ${record.stream} ${record.text}`).join('')) }
    catch (err) { notifyError('logs', errorMessage(err, 'Unable to copy logs')) }
  }
  function reconnect() {
    setRecords([])
    setDropped(0)
    setMessage('')
    setConnection('Connecting')
    setFollow(true)
    setGeneration((value) => value + 1)
  }
  return <section className="logConsole">
    <div className="consoleHeader"><h3>Container logs</h3><span role="status">{follow ? connection : 'Paused'}</span></div>
    <div className="inspectorForm inline">
      <label><span>Search logs</span><input type="search" value={query} onChange={(event) => setQuery(event.target.value)} /></label>
      <label><span>Log stream</span><select value={stream} onChange={(event) => setStream(event.target.value)}><option value="all">All streams</option><option value="stdout">stdout</option><option value="stderr">stderr</option><option value="combined">Legacy combined</option></select></label>
      <button type="button" onClick={() => { if (follow) setFollow(false); else reconnect() }}>{follow ? 'Pause logs' : 'Resume logs'}</button>
      <button type="button" onClick={reconnect}>Reconnect logs</button>
      <button type="button" disabled={!filtered.length} onClick={() => void copy()}>Copy logs</button>
      <button type="button" onClick={reconnect}>Clear buffer</button>
    </div>
    {message && <p className="hintLine" role="status">{message}</p>}
    <p className="hintLine">At most 5,000 records / 4 MiB retained · {dropped} older record(s) discarded. Reconnecting resets the buffer; captured timestamps are not invented for legacy logs.</p>
    <div className="logViewport" role="log" aria-label="Container output" aria-live="off" tabIndex={0}>
      {filtered.map((record) => <div className={`logLine ${record.stream === 'stderr' ? 'stderr' : 'stdout'}`} key={record.sequence}>
        <time dateTime={record.timestamp}>{record.timestamp ? new Date(record.timestamp).toLocaleTimeString([], { hour12: false }) : 'untimed'}</time>
        <span className="streamLabel">{record.stream}</span><span className="logMessage">{record.text}</span>
      </div>)}
    </div>
  </section>
}

export function ContainerStats({ id, running }: { id: string; running: boolean }) {
  const stats = usePolledResource<InspectorStats>(
    (signal) => apiGet(`/api/docker/containers/${encodeURIComponent(id)}/stats`, signal), running ? 3000 : 0,
    [id, running], `docker:stats:${id}`,
  )
  const point = stats.data?.current
  const memory = (stats.data?.history ?? []).map((entry) => entry.memoryBytes).filter((value): value is number => value !== undefined)
  const maximum = Math.max(1, ...memory)
  const plot = memory.map((value, index) => `${index * 300 / Math.max(1, memory.length - 1)},${80 - value / maximum * 75}`).join(' ')
  return <section className="drawerPanel">
    <h3>Live and recent statistics</h3>
    {!running && <p className="hintLine">Live metrics require a running task. Previously collected samples remain until the daemon restarts.</p>}
    {stats.error && <p role="alert" className="errorLine">Disconnected / unavailable: {stats.error}</p>}
    {stats.data && !stats.data.available && <p role="status">Live task disconnected or stopped; showing retained samples.</p>}
    <dl className="runtimeGrid">
      <div><dt>CPU</dt><dd>{point?.cpuMillicores === undefined ? 'Unavailable' : `${point.cpuMillicores}m`}</dd></div>
      <div><dt>Memory</dt><dd>{bytesLabel(point?.memoryBytes)} / {bytesLabel(point?.memoryLimit)}</dd></div>
      <div><dt>Processes</dt><dd>{point?.pids ?? 'Unavailable'}</dd></div>
      <div><dt>Network RX / TX</dt><dd>{bytesLabel(point?.networkRX)} / {bytesLabel(point?.networkTX)}</dd></div>
      <div><dt>Block read / write</dt><dd>{bytesLabel(point?.blockRead)} / {bytesLabel(point?.blockWrite)}</dd></div>
    </dl>
    {memory.length > 0 && <svg className="statsHistory" viewBox="0 0 300 85" role="img" aria-label="Recent memory history"><polyline points={plot} fill="none" stroke="currentColor" strokeWidth="2" /></svg>}
    <p className="hintLine">{stats.data?.history.length ?? 0} recent sample(s). {stats.data?.message}</p>
  </section>
}

const SENSITIVE = /password|token|secret|credential|authorization|private.?key|api.?key/i
function object(value: JSONValue | undefined): { [key: string]: JSONValue } {
  return value !== null && typeof value === 'object' && !Array.isArray(value) ? value : {}
}
function redacted(value: JSONValue, key = ''): JSONValue {
  if (SENSITIVE.test(key)) return '[redacted]'
  if (Array.isArray(value)) return value.map((item) => {
    if (key.toLowerCase() === 'env' && typeof item === 'string') {
      const name = item.split('=', 1)[0]
      return SENSITIVE.test(name) ? `${name}=[redacted]` : item
    }
    return redacted(item)
  })
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([name, item]) => [name, redacted(item, name)]))
  return value
}

export function ContainerInspect({ id }: { id: string }) {
  const { notifyError } = useMessages()
  const [revealed, setRevealed] = useState(false)
  const inspect = usePolledResource<JSONValue>((signal) => apiGet(`/api/docker/containers/${encodeURIComponent(id)}`, signal), 0, [id])
  const original = inspect.data
  const document = original === null ? null : revealed ? original : redacted(original)
  const config = object(object(document ?? {}).Config)
  const host = object(object(document ?? {}).HostConfig)
  async function copy() {
    try { await writeClipboard(JSON.stringify(document, null, 2)) }
    catch (err) { notifyError('inspect', errorMessage(err, 'Unable to copy inspect data')) }
  }
  return <section className="drawerPanel">
    <h3>Docker / OCI metadata</h3>
    {inspect.error && <p role="alert" className="errorLine">{inspect.error}</p>}
    <div className="dataActions">
      <button type="button" aria-pressed={revealed} onClick={() => { if (revealed || window.confirm('Reveal sensitive environment and label values already accessible through your Docker socket?')) setRevealed((value) => !value) }}>{revealed ? 'Hide sensitive values' : 'Reveal sensitive values'}</button>
      <button type="button" disabled={document === null} onClick={() => void copy()}>Copy inspect JSON</button>
      <button type="button" onClick={inspect.reload}>Refresh inspect</button>
    </div>
    {document !== null && <>
      {[
        ['Environment', config.Env], ['Labels', config.Labels], ['Mounts', object(document).Mounts],
        ['Networks', object(document).NetworkSettings], ['Health', object(object(document).State).Health ?? config.Healthcheck],
        ['Restart configuration', host.RestartPolicy],
      ].map(([label, value]) => <details key={String(label)}><summary>{String(label)}</summary><pre className="logRaw">{JSON.stringify(value ?? null, null, 2)}</pre></details>)}
      <details><summary>Raw inspect JSON</summary><pre className="logRaw">{JSON.stringify(document, null, 2)}</pre></details>
    </>}
  </section>
}
