import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, apiUpload, errorMessage, isAbortError } from '../api'
import { bytesLabel, type BackupSchedule, type DataOperation, type DataRequest, type FileResource, type StorageResource, type VolumeArchive, type VolumePreview } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from './DataOperations'
import { VolumeOwnership } from './VolumeOwnership'

export function VolumeTools({ name = '', initialAction }: { name?: string; initialAction?: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [action, setAction] = useState(initialAction || (name ? 'volume-export' : 'volume-import'))
  const [destination, setDestination] = useState('')
  const [archive, setArchive] = useState<VolumeArchive | null>(null)
  const [preview, setPreview] = useState<VolumePreview | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const [intervalHours, setIntervalHours] = useState(24)
  const [retention, setRetention] = useState(7)
  const [directory, setDirectory] = useState('')
  const [editingSchedule, setEditingSchedule] = useState<BackupSchedule | null>(null)
  const controller = useRef<AbortController | null>(null)
  useEffect(() => () => controller.current?.abort(), [])
  const source = usePolledResource<StorageResource | null>((signal) => name
    ? apiGet(`/api/docker/volumes/${encodeURIComponent(name)}/storage`, signal) : Promise.resolve(null),
  name ? 10000 : 0, [name], name ? `docker:volume-storage:${name}` : undefined)
  const schedules = usePolledResource<BackupSchedule[]>((signal) => apiGet('/api/docker/backups', signal), 5000, [], 'data:backups')
  const matching = (schedules.data ?? []).filter((schedule) => name && schedule.resource.name === name)
  const sourceResource: FileResource | undefined = source.data ? { ...source.data.resource, kind: 'volume' } : undefined

  async function run(work: (signal: AbortSignal) => Promise<void>) {
    controller.current?.abort()
    const current = new AbortController()
    controller.current = current
    setBusy(true)
    setFailure('')
    try { await work(current.signal) }
    catch (err) {
      if (!isAbortError(err)) { const message = errorMessage(err, 'Volume operation failed'); setFailure(message); notifyError('volumes', message) }
    } finally { if (controller.current === current) setBusy(false) }
  }
  async function prepare() {
    setPreview(null)
    const request: DataRequest = {
      action, resource: action === 'volume-import' ? undefined : sourceResource,
      identity: action === 'volume-import' ? undefined : source.data?.identity,
      destination: action === 'volume-clone' || action === 'volume-import' ? destination : undefined,
      archive: action === 'volume-import' || action === 'volume-restore' ? archive?.path : undefined,
    }
    await run(async (signal) => { setPreview(await apiSend<VolumePreview>('/api/docker/storage/preview', 'POST', request, signal)) })
  }
  async function execute() {
    if (!preview || !window.confirm(`${preview.consequences}\n\nSource: ${preview.resource.name || preview.archive?.path}\nDestination: ${preview.destination || 'managed local archive'}`)) return
    await run(async (signal) => {
      const operation = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token }, signal)
      notifyNotice('volumes', `Started data operation #${operation.id}. Track progress and cancel in history or Activity.`)
      setPreview(null)
    })
  }
  function upload(file: File | undefined) {
    setArchive(null)
    setPreview(null)
    if (!file) return
    void run(async (signal) => { setArchive(await apiUpload<VolumeArchive>('/api/data/archives', file, signal)) })
  }
  async function saveSchedule(enabled: boolean, existing?: BackupSchedule, edit = false) {
    if (!sourceResource && !existing) return
    await run(async (signal) => {
      const schedule = existing
        ? { ...existing, enabled, ...(edit ? { intervalHours, retention, directory } : {}) }
        : { id: 0, resource: sourceResource, enabled, intervalHours, retention, directory, nextRunAt: '' }
      await apiSend('/api/docker/backups', 'POST', schedule, signal)
      setEditingSchedule(null)
      schedules.reload()
      notifyNotice('backups', edit ? 'Local backup policy updated.' : enabled ? 'Local backup schedule enabled.' : 'Local backup schedule paused.')
    })
  }
  async function removeSchedule(schedule: BackupSchedule) {
    if (!window.confirm(`Remove backup schedule ${schedule.id} for ${schedule.resource.name}? Existing verified archives and their history remain intact.`)) return
    await run(async (signal) => {
      await apiSend(`/api/docker/backups/${schedule.id}?confirm=true`, 'DELETE', undefined, signal)
      setEditingSchedule(null)
      schedules.reload()
      notifyNotice('backups', 'Backup schedule removed; existing archives retained.')
    })
  }
  async function backupNow(schedule: BackupSchedule) {
    await run(async (signal) => {
      const operation = await apiSend<DataOperation>(`/api/docker/backups/${schedule.id}/run`, 'POST', undefined, signal)
      notifyNotice('backups', `Started crash-consistent backup #${operation.id}.`)
    })
  }
  function editSchedule(schedule: BackupSchedule) {
    setIntervalHours(schedule.intervalHours)
    setRetention(schedule.retention)
    setDirectory(schedule.directory)
    setEditingSchedule(schedule)
  }
  const previewChanged = Boolean(preview && action !== 'volume-import' && source.data && source.data.identity !== preview.request.identity)

  return <>
    {source.data && <VolumeOwnership volume={source.data} />}
    <section className="drawerPanel">
      <h3>Volume lifecycle and transfer</h3>
      <p className="hintLine">Archives are local, integrity-checked and crash-consistent, not application-consistent database backups. Stop or quiesce writers explicitly when required. Managed-cluster data and active mounts are protected by the backend.</p>
      {(source.error || failure) && <p role="alert" className="errorLine">{source.error || failure}</p>}
      <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void prepare() }}>
        <label><span>Volume action</span><select disabled={busy} value={action} onChange={(event) => { setAction(event.target.value); setPreview(null) }}>
          {name && <><option value="volume-export">Export local archive</option><option value="volume-clone">Clone to new volume</option></>}
          <option value="volume-import">Import archive to new volume</option>
          {name && <><option value="volume-restore">Restore this volume</option><option value="volume-empty">Empty this volume</option><option value="volume-remove">Remove this volume</option></>}
        </select></label>
        {(action === 'volume-clone' || action === 'volume-import') && <label><span>New destination volume</span><input type="text" value={destination} disabled={busy} maxLength={128} pattern="[A-Za-z0-9][A-Za-z0-9_.-]*" onChange={(event) => { setDestination(event.target.value); setPreview(null) }} required /></label>}
        {action === 'volume-export' && <p>Export publishes a verified managed archive. Download it from the completed operation and choose its local destination in your browser.</p>}
        {(action === 'volume-import' || action === 'volume-restore') && <label><span>Versioned volume archive</span><input type="file" accept=".tar" disabled={busy} onChange={(event) => upload(event.target.files?.[0])} /></label>}
        {archive && <p>{archive.path} / {bytesLabel(archive.bytes)} / verified SHA-256 {archive.sha256.slice(0, 16)}</p>}
        <button type="submit" disabled={busy || ((!source.data || Boolean(source.error)) && action !== 'volume-import') || ((action === 'volume-import' || action === 'volume-restore') && !archive)}>{busy ? 'Preparing...' : 'Preview volume action'}</button>
        {busy && <button type="button" onClick={() => { controller.current?.abort(); setBusy(false) }}>Cancel preparation</button>}
      </form>
      {preview && <section aria-label="Volume action preview">
        <h4>Exact source and destination</h4><p>{preview.resource.name || preview.archive?.path} → {preview.destination || 'managed local archive'}</p>
        <p>{preview.consequences}</p>
        {preview.owners.map((owner) => <p key={owner.id}>{owner.name} / {owner.state} / {owner.composeProject ? `${owner.composeProject}/${owner.composeService || ''}` : 'Standalone'}{owner.managed ? ' / managed Kubernetes' : ''}</p>)}
        {preview.files && <details><summary>{preview.files.length} file / directory entries to remove</summary><ul>{preview.files.map((file) => <li key={file.path}>{file.path} / {file.type} / {bytesLabel(file.size)}</li>)}</ul></details>}
        {previewChanged && <p role="alert" className="errorLine">The source volume identity changed. Preview again.</p>}
        <button type="button" className="destructiveAction" disabled={busy || previewChanged || (Boolean(source.error) && action !== 'volume-import')} onClick={() => void execute()}>Confirm previewed volume action</button>
      </section>}
    </section>
    {name && <section className="drawerPanel">
      <h3>Scheduled local backups</h3>
      <p className="hintLine">Backups remain in Porto's local state directory. Retention runs only after a verified replacement has a durable recovery record. No account or cloud service is required.</p>
      {schedules.error && <p role="alert" className="errorLine">{schedules.error}</p>}
      {(!matching.length || editingSchedule) && <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void saveSchedule(editingSchedule?.enabled ?? true, editingSchedule ?? undefined, Boolean(editingSchedule)) }}>
        <label><span>Interval in hours</span><input type="number" required min={1} max={8760} disabled={busy} value={intervalHours} onChange={(event) => setIntervalHours(Number(event.target.value))} /></label>
        <label><span>Verified archives to retain</span><input type="number" required min={1} max={365} disabled={busy} value={retention} onChange={(event) => setRetention(Number(event.target.value))} /></label>
        <label><span>Local backup folder inside Porto state (blank uses a source-specific folder)</span><input type="text" disabled={busy} value={directory} onChange={(event) => setDirectory(event.target.value)} /></label>
        <button type="submit" disabled={busy || !source.data || Boolean(source.error)}>{editingSchedule ? 'Save backup policy' : 'Enable local backups'}</button>
        {editingSchedule && <button type="button" disabled={busy} onClick={() => setEditingSchedule(null)}>Cancel policy edit</button>}
      </form>}
      {matching.map((schedule) => <div key={schedule.id}>
        <p>{schedule.enabled ? 'Enabled' : 'Paused'} / every {schedule.intervalHours}h / retain {schedule.retention} / {schedule.directory}</p>
        <p>Next deadline: {new Date(schedule.nextRunAt).toLocaleString()}. Missed jobs run once, without overlap or catch-up storms.</p>
        <div className="dataActions">
          <button type="button" disabled={busy} onClick={() => editSchedule(schedule)}>Edit backup policy</button>
          <button type="button" disabled={busy} onClick={() => void saveSchedule(!schedule.enabled, schedule)}>{schedule.enabled ? 'Pause backups' : 'Resume backups'}</button>
          <button type="button" disabled={busy} onClick={() => void backupNow(schedule)}>Back up now</button>
          <button type="button" className="destructiveAction" disabled={busy} onClick={() => void removeSchedule(schedule)}>Remove backup schedule</button>
        </div>
      </div>)}
    </section>}
    <DataOperations resourceName={name || undefined} onComplete={source.reload} />
  </>
}
