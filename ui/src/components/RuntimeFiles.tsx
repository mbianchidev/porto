import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, apiUpload, errorMessage, isAbortError } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { bytesLabel, type FileContent, type FileEntry, type FileListing } from '../dataTypes'
import { NativeFiles } from './NativeFiles'

const PLURAL = { container: 'containers', image: 'images', volume: 'volumes' } as const

export function RuntimeFiles({ kind, name }: { kind: keyof typeof PLURAL; name: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [path, setPath] = useState('.')
  const [pathDraft, setPathDraft] = useState('.')
  const [file, setFile] = useState<FileContent | null>(null)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const identity = useRef('')
  const operation = useRef<AbortController | null>(null)
  const prefix = `/api/docker/${PLURAL[kind]}/${encodeURIComponent(name)}`
  useEffect(() => () => operation.current?.abort(), [])
  const listing = usePolledResource<FileListing>(async (signal) => {
    const params = new URLSearchParams({ path })
    if (identity.current) params.set('identity', identity.current)
    const result = await apiGet<FileListing>(`${prefix}/files?${params}`, signal)
    identity.current = result.identity
    return result
  }, 5000, [prefix, path])

  function fileURL(relative: string, suffix = '', checksum = '') {
    const params = new URLSearchParams({ path: relative, identity: listing.data?.identity ?? '' })
    if (checksum) params.set('sha256', checksum)
    return `${prefix}/file${suffix}?${params}`
  }

  async function run(action: (signal: AbortSignal) => Promise<void>) {
    operation.current?.abort()
    const controller = new AbortController()
    operation.current = controller
    setBusy(true)
    setFailure('')
    try {
      await action(controller.signal)
    } catch (err) {
      if (!isAbortError(err)) {
        const message = errorMessage(err, 'Filesystem operation failed')
        setFailure(message)
        notifyError('files', message)
      }
    } finally {
      if (!controller.signal.aborted) setBusy(false)
    }
  }

  function navigate(relative: string) {
    if (file && text !== file.text && !window.confirm('Discard the unsaved file edit?')) return
    operation.current?.abort()
    setBusy(false)
    setFile(null)
    setPath(relative)
    setPathDraft(relative)
  }

  function select(entry: FileEntry) {
    if (entry.type === 'directory') {
      navigate(entry.path)
      return
    }
    if (entry.type !== 'file') {
      setFailure('Links and special files are shown but not traversed. Download or transfer a supported regular file instead.')
      return
    }
    void run(async (signal) => {
      const content = await apiGet<FileContent>(fileURL(entry.path), signal)
      if (!signal.aborted) { setFile(content); setText(content.text) }
    })
  }

  async function save() {
    if (!file || file.readOnly || !window.confirm(`Replace the contents of ${file.path}? Concurrent changes will be rejected.`)) return
    await run(async (signal) => {
      await apiUpload(fileURL(file.path, '', file.sha256), text, signal)
      const current = await apiGet<FileContent>(fileURL(file.path), signal)
      if (!signal.aborted) { setFile(current); setText(current.text) }
      listing.reload()
      notifyNotice('files', `Saved ${file.path}.`)
    })
  }

  async function remove() {
    if (!file || file.readOnly || !window.confirm(`Delete ${file.path} permanently? This removes only this previewed file.`)) return
    await run(async (signal) => {
      await apiSend(`${fileURL(file.path, '', file.sha256)}&confirm=true`, 'DELETE', undefined, signal)
      setFile(null)
      listing.reload()
      notifyNotice('files', 'Deleted the previewed file.')
    })
  }

  function upload(uploaded: File | undefined) {
    if (!uploaded) return
    if (uploaded.size > 64 * 1024 * 1024) { setFailure('Uploads are limited to 64 MiB. Use a volume archive for larger data.'); return }
    const relative = path === '.' ? uploaded.name : `${path}/${uploaded.name}`
    void run(async (signal) => {
      await apiUpload(fileURL(relative), uploaded, signal)
      listing.reload()
      notifyNotice('files', `Uploaded ${uploaded.name}; existing files are never overwritten implicitly.`)
    })
  }

  const unavailable = Boolean(listing.error)
  return (
    <><NativeFiles kind={kind} name={name} /><section className="drawerPanel runtimeFiles">
      <h3>Files</h3>
      <p className="hintLine">Text previews/edits: 256 KiB. File transfers: 64 MiB. Symlinks cannot escape the selected resource. Uploads create new files; edits replace files atomically.</p>
      {listing.data?.message && <p className="hintLine">{listing.data.message}</p>}
      <form className="inspectorForm inline" onSubmit={(event) => { event.preventDefault(); navigate(pathDraft.replace(/^\/+/, '') || '.') }}>
        <label><span>Directory path</span><input type="text" value={pathDraft} onChange={(event) => setPathDraft(event.target.value)} /></label>
        <button type="submit" disabled={busy}>Browse</button>
        <button type="button" disabled={path === '.' || busy} onClick={() => navigate(path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '.')}>Parent</button>
        <button type="button" disabled={busy} onClick={listing.reload}>Refresh files</button>
        <label><span>Upload new file</span><input type="file" disabled={busy || unavailable || !listing.data || listing.data.readOnly} onChange={(event) => { upload(event.target.files?.[0]); event.target.value = '' }} /></label>
      </form>
      {(listing.error || failure) && <p role="alert" className="errorLine">{listing.error || failure}</p>}
      {unavailable && <button type="button" onClick={() => { identity.current = ''; listing.update(() => null); setFile(null); listing.reload() }}>Reconnect to current resource</button>}
      {listing.loading && !listing.data && <p role="status">Reading filesystem capability…</p>}
      {listing.data && !unavailable && <>
        <p className="hintLine">{listing.data.readOnly ? 'Read-only access' : 'Writable access; edits are explicit'} · identity {listing.data.identity.slice(0, 12)}</p>
        <div className="dataTableScroll"><table className="dataTable">
          <thead><tr><th>Name</th><th>Type</th><th>Size</th><th>Mode / UID:GID</th></tr></thead>
          <tbody>{listing.data.entries.map((entry) => <tr key={entry.path}>
            <td><button type="button" disabled={busy} onClick={() => select(entry)}>{entry.path.split('/').pop()}</button>{entry.linkTarget && <small> → {entry.linkTarget}</small>}</td>
            <td>{entry.type}</td><td>{bytesLabel(entry.size)}</td><td>{entry.mode.toString(8).padStart(4, '0')} / {entry.uid}:{entry.gid}</td>
          </tr>)}</tbody>
        </table></div>
        {listing.data.entries.length === 0 && <p>Directory is empty.</p>}
        {listing.data.truncated && <p role="status">Listing reached its bound. Enter a narrower directory path to continue.</p>}
        {listing.data.mounts?.length ? <details><summary>Mount boundaries</summary><dl className="runtimeGrid">{listing.data.mounts.map((mount) => <div key={mount.path}><dt>{mount.path}</dt><dd>{mount.kind} · {mount.readOnly ? 'read-only' : 'writable'} · {mount.source}</dd></div>)}</dl></details> : null}
      </>}
      {file && !unavailable && <section className="fileEditor">
        <h4>{file.path}</h4>
        <label><span className="visuallyHidden">File contents</span><textarea value={text} readOnly={file.readOnly} onChange={(event) => setText(event.target.value)} rows={14} /></label>
        <div className="dataActions">
          <button type="button" disabled={busy || file.readOnly || text === file.text} onClick={() => void save()}>Save file</button>
          <a href={fileURL(file.path, '/download')} download={file.path.split('/').pop()}>Download file</a>
          <button type="button" className="destructiveAction" disabled={busy || file.readOnly} onClick={() => void remove()}>Delete file</button>
          {busy && <button type="button" onClick={() => { operation.current?.abort(); setBusy(false) }}>Cancel file operation</button>}
        </div>
      </section>}
    </section></>
  )
}
