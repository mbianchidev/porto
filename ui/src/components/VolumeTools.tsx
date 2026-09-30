import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, apiUpload, errorMessage, isAbortError } from '../api'
import { bytesLabel, type BackupSchedule, type DataOperation, type DataRequest, type FileListing, type VolumeArchive, type VolumePreview } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from './DataOperations'

export function VolumeTools({ name }: { name: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [action, setAction] = useState('volume-export')
  const [destination, setDestination] = useState('')
  const [archive, setArchive] = useState<VolumeArchive | null>(null)
  const [preview, setPreview] = useState<VolumePreview | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const [intervalHours, setIntervalHours] = useState(24)
  const [retention, setRetention] = useState(7)
  const [directory, setDirectory] = useState('')
  const controller = useRef<AbortController | null>(null)
  useEffect(() => () => controller.current?.abort(), [])
  const source = usePolledResource<FileListing>((signal) => apiGet(`/api/docker/volumes/${encodeURIComponent(name)}/files?path=.`, signal), 0, [name])
  const schedules = usePolledResource<BackupSchedule[]>((signal) => apiGet('/api/docker/backups', signal), 5000, [], 'data:backups')
  const matching = (schedules.data ?? []).filter((schedule) => schedule.resource.name === name)

  async function run(work: (signal: AbortSignal) => Promise<void>) {
    controller.current?.abort()
    const current = new AbortController()
    controller.current = current
    setBusy(true)
    setFailure('')
    try { await work(current.signal) }
    catch (err) {
      if (!isAbortError(err)) { const message = errorMessage(err, 'Volume operation failed'); setFailure(message); notifyError('volumes', message) }
    } finally { if (!current.signal.aborted) setBusy(false) }
  }
  async function prepare() {
    const request: DataRequest = {
      action, resource: source.data?.resource, identity: source.data?.identity,
      destination, archive: archive?.path,
    }
    await run(async (signal) => { setPreview(await apiSend<VolumePreview>('/api/docker/storage/preview', 'POST', request, signal)) })
  }
  async function execute() {
    if (!preview || !window.confirm(`${preview.consequences}\n\nSource: ${preview.resource.name || preview.archive?.path}\nDestination: ${preview.destination || 'managed local archive'}`)) return
    await run(async (signal) => {
      const operation = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token }, signal)
      notifyNotice('volumes', `Started data operation #${operation.id}. Track progress and cancel in history.`)
      setPreview(null)
    })
  }
  function upload(file: File | undefined) {
    if (!file) return
    void run(async (signal) => { setArchive(await apiUpload<VolumeArchive>('/api/data/archives', file, signal)); setPreview(null) })
  }
  async function saveSchedule(enabled: boolean, existing?: BackupSchedule) {
    if (!source.data) return
    await run(async (signal) => {
      await apiSend('/api/docker/backups', 'POST', existing
        ? { ...existing, enabled }
        : { id: 0, resource: source.data?.resource, enabled, intervalHours, retention, directory, nextRunAt: '' }, signal)
      schedules.reload()
      notifyNotice('backups', enabled ? 'Local backup schedule enabled.' : 'Local backup schedule paused.')
    })
  }
  async function backupNow(schedule: BackupSchedule) {
    await run(async (signal) => {
      const operation = await apiSend<DataOperation>(`/api/docker/backups/${schedule.id}/run`, 'POST', undefined, signal)
      notifyNotice('backups', `Started backup #${operation.id}.`)
    })
  }
  return <>
    <section className="drawerPanel">
      <h3>Volume lifecycle and transfer</h3>
      <p className="hintLine">Archives are local, integrity-checked and crash-consistent. They are not application-consistent database backups. Stop or quiesce writers yourself when consistency requires it.</p>
      {(source.error || failure) && <p role="alert" className="errorLine">{source.error || failure}</p>}
      <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void prepare() }}>
        <label><span>Volume action</span><select value={action} onChange={(event) => { setAction(event.target.value); setPreview(null) }}>
          <option value="volume-export">Export local archive</option><option value="volume-clone">Clone to new volume</option>
          <option value="volume-import">Import archive to new volume</option><option value="volume-restore">Restore this volume</option><option value="volume-empty">Empty this volume</option>
        </select></label>
        {action !== 'volume-empty' && action !== 'volume-restore' && <label><span>{action === 'volume-export' ? 'Absolute export path (blank uses managed archives)' : 'New destination volume'}</span><input type="text" value={destination} onChange={(event) => { setDestination(event.target.value); setPreview(null) }} required={action !== 'volume-export'} /></label>}
        {(action === 'volume-import' || action === 'volume-restore') && <label><span>Versioned volume archive</span><input type="file" accept=".tar" disabled={busy} onChange={(event) => upload(event.target.files?.[0])} /></label>}
        {archive && <p>{archive.path} · {bytesLabel(archive.bytes)} · verified SHA-256 {archive.sha256.slice(0, 16)}</p>}
        <button type="submit" disabled={busy || (!source.data && action !== 'volume-import') || ((action === 'volume-import' || action === 'volume-restore') && !archive)}>{busy ? 'Preparing…' : 'Preview volume action'}</button>
        {busy && <button type="button" onClick={() => { controller.current?.abort(); setBusy(false) }}>Cancel preparation</button>}
      </form>
      {preview && <section aria-label="Volume action preview">
        <h4>Exact source and destination</h4><p>{preview.resource.name || preview.archive?.path} → {preview.destination || 'managed local archive'}</p>
        <p>{preview.consequences}</p>
        <dl className="runtimeGrid">{preview.owners.map((owner) => <div key={owner.id}><dt>{owner.name}</dt><dd>{owner.state} · {owner.composeProject || 'standalone'} · {owner.composeService}</dd></div>)}</dl>
        {preview.files && <details><summary>{preview.files.length} file / directory entries to remove</summary><ul>{preview.files.map((file) => <li key={file.path}>{file.path} · {file.type} · {bytesLabel(file.size)}</li>)}</ul></details>}
        <button type="button" className="destructiveAction" disabled={busy} onClick={() => void execute()}>Confirm previewed volume action</button>
      </section>}
    </section>
    <section className="drawerPanel">
      <h3>Scheduled local backups</h3>
      {schedules.error && <p className="errorLine">{schedules.error}</p>}
      {!matching.length && <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void saveSchedule(true) }}>
        <label><span>Interval in hours</span><input type="number" min={1} max={8760} value={intervalHours} onChange={(event) => setIntervalHours(Number(event.target.value))} /></label>
        <label><span>Verified archives to retain</span><input type="number" min={1} max={365} value={retention} onChange={(event) => setRetention(Number(event.target.value))} /></label>
        <label><span>Absolute local backup directory (blank uses Porto state)</span><input type="text" value={directory} onChange={(event) => setDirectory(event.target.value)} /></label>
        <button type="submit" disabled={busy || !source.data}>Enable local backups</button>
      </form>}
      {matching.map((schedule) => <div key={schedule.id}>
        <p>{schedule.enabled ? 'Enabled' : 'Paused'} · every {schedule.intervalHours}h · retain {schedule.retention} · {schedule.directory}</p>
        <p>Next deadline: {new Date(schedule.nextRunAt).toLocaleString()}. Missed jobs run once, without overlap or catch-up storms.</p>
        <div className="dataActions"><button type="button" disabled={busy} onClick={() => void saveSchedule(!schedule.enabled, schedule)}>{schedule.enabled ? 'Pause backups' : 'Resume backups'}</button><button type="button" disabled={busy} onClick={() => void backupNow(schedule)}>Back up now</button></div>
      </div>)}
    </section>
    <DataOperations resourceName={name} />
  </>
}
