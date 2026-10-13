import { useEffect, useRef, useState } from 'react'
import { apiDownload, apiGet, apiSend, apiUpload, errorMessage, isAbortError } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { bytesLabel, MAX_FILE_TRANSFER_BYTES, MAX_TEXT_BYTES, type FileContent, type FileEntry, type FileListing } from '../dataTypes'
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
  useEffect(() => () => operation.current?.abort(), [prefix])
  const listing = usePolledResource<FileListing>(async (signal) => {
    const params = new URLSearchParams({ path })
    if (identity.current) params.set('identity', identity.current)
    const result = await apiGet<FileListing>(`${prefix}/files?${params}`, signal)
    signal.throwIfAborted()
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
      if (operation.current === controller) {
        operation.current = null
        if (!controller.signal.aborted) setBusy(false)
      }
    }
  }

  function cancelOperation() {
    operation.current?.abort()
    operation.current = null
    setBusy(false)
  }

  function discardChanges() {
    return !file || text === file.text || window.confirm('Discard the unsaved file edit?')
  }

  function navigate(relative: string) {
    if (!discardChanges()) return
    cancelOperation()
    setFile(null)
    setText('')
    setFailure('')
    listing.update(() => null)
    setPath(relative)
    setPathDraft(relative)
    if (relative === path) listing.reload()
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
    if (!discardChanges()) return
    setFile(null)
    setText('')
    void run(async (signal) => {
      const content = await apiGet<FileContent>(fileURL(entry.path), signal)
      if (!signal.aborted) { setFile(content); setText(content.text) }
    })
  }

  async function save() {
    if (!file || file.readOnly) return
    if (textError) { setFailure(textError); return }
    if (!window.confirm(`Replace the contents of ${file.path}? Concurrent changes will be rejected.`)) return
    await run(async (signal) => {
      await apiUpload(fileURL(file.path, '', file.sha256), text, signal)
      signal.throwIfAborted()
      const current = await apiGet<FileContent>(fileURL(file.path), signal)
      signal.throwIfAborted()
      setFile(current)
      setText(current.text)
      listing.reload()
      notifyNotice('files', `Saved ${file.path}.`)
    })
  }

  async function remove() {
    if (!file || file.readOnly || !window.confirm(`Delete ${file.path} permanently? This removes only this previewed file.`)) return
    await run(async (signal) => {
      await apiSend(`${fileURL(file.path, '', file.sha256)}&confirm=true`, 'DELETE', undefined, signal)
      signal.throwIfAborted()
      setFile(null)
      setText('')
      listing.reload()
      notifyNotice('files', 'Deleted the previewed file.')
    })
  }

  function upload(uploaded: File | undefined) {
    if (!uploaded) return
    if (uploaded.size > MAX_FILE_TRANSFER_BYTES) { setFailure('Uploads are limited to 64 MiB. Use a volume archive for larger data.'); return }
    const relative = path === '.' ? uploaded.name : `${path}/${uploaded.name}`
    void run(async (signal) => {
      await apiUpload(fileURL(relative), uploaded, signal)
      signal.throwIfAborted()
      listing.reload()
      notifyNotice('files', `Uploaded ${uploaded.name}; existing files are never overwritten implicitly.`)
    })
  }

  async function download(relative: string) {
    await run(async (signal) => {
      const blob = await apiDownload(fileURL(relative, '/download'), signal)
      signal.throwIfAborted()
      if (blob.size > MAX_FILE_TRANSFER_BYTES) throw new Error('Downloads are limited to 64 MiB. Use an archive for larger data.')
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = relative.split('/').pop() ?? 'download'
      document.body.appendChild(anchor)
      try { anchor.click() }
      finally {
        anchor.remove()
        window.setTimeout(() => URL.revokeObjectURL(url), 0)
      }
    })
  }

  const unavailable = Boolean(listing.error)
  const textError = new Blob([text]).size > MAX_TEXT_BYTES
    ? 'Text edits are limited to 256 KiB of UTF-8 text.'
    : text.includes('\0') ? 'Text edits cannot contain NUL bytes.' : ''
  return (
    <><NativeFiles kind={kind} name={name} /><section className="drawerPanel runtimeFiles">
      <h3>Files</h3>
      <p className="hintLine">Text previews/edits: 256 KiB. File transfers: 64 MiB. Binary and larger files can be downloaded directly; editing/deletion requires a bounded text preview. Symlinks cannot escape the selected resource. Uploads create new files; edits replace files atomically.</p>
      {listing.data?.message && <p className="hintLine">{listing.data.message}</p>}
      <form className="inspectorForm inline" onSubmit={(event) => { event.preventDefault(); navigate(pathDraft.replace(/^\/+/, '') || '.') }}>
        <label><span>Directory path</span><input type="text" value={pathDraft} onChange={(event) => setPathDraft(event.target.value)} /></label>
        <button type="submit" disabled={busy}>Browse</button>
        <button type="button" disabled={path === '.' || busy} onClick={() => navigate(path.includes('/') ? path.slice(0, path.lastIndexOf('/')) : '.')}>Parent</button>
        <button type="button" disabled={busy} onClick={listing.reload}>Refresh files</button>
        <label><span>Upload new file</span><input type="file" disabled={busy || unavailable || !listing.data || listing.data.readOnly} onChange={(event) => { upload(event.target.files?.[0]); event.target.value = '' }} /></label>
      </form>
      {(listing.error || failure) && <p role="alert" className="errorLine">{listing.error || failure}</p>}
      {unavailable && <button type="button" onClick={() => {
        if (!discardChanges()) return
        cancelOperation()
        identity.current = ''
        listing.update(() => null)
        setFile(null)
        setText('')
        setFailure('')
        listing.reload()
      }}>Reconnect to current resource</button>}
      {busy && <div className="dataActions" role="status">
        <span>Filesystem operation in progress.</span>
        <button type="button" onClick={cancelOperation}>Cancel file operation</button>
      </div>}
      {listing.loading && !listing.data && <p role="status">Reading filesystem capability…</p>}
      {listing.data && !unavailable && <>
        <p className="hintLine">{listing.data.readOnly ? 'Read-only access' : 'Writable access; edits are explicit'} · identity {listing.data.identity.slice(0, 12)}</p>
        <div className="dataTableScroll"><table className="dataTable">
          <thead><tr><th>Name</th><th>Type</th><th>Size</th><th>Mode / UID:GID</th></tr></thead>
          <tbody>{listing.data.entries.map((entry) => <tr key={entry.path}>
            <td><div className="runtimeFileActions">
              <button type="button" title={entry.path} disabled={busy} onClick={() => select(entry)}>{entry.path.split('/').pop()}</button>
              {entry.type === 'file' && (entry.size <= MAX_FILE_TRANSFER_BYTES
                ? <button type="button" aria-label={`Download ${entry.path}`} disabled={busy} onClick={() => void download(entry.path)}>Download</button>
                : <small>Over 64 MiB; use an archive.</small>)}
            </div>{entry.linkTarget && <small> → {entry.linkTarget}</small>}</td>
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
        {textError && <p className="errorLine" role="alert">{textError}</p>}
        <div className="dataActions">
          <button type="button" disabled={busy || file.readOnly || Boolean(textError) || text === file.text} onClick={() => void save()}>Save file</button>
          <button type="button" disabled={busy} onClick={() => void download(file.path)}>Download file</button>
          <button type="button" className="destructiveAction" disabled={busy || file.readOnly} onClick={() => void remove()}>Delete file</button>
        </div>
      </section>}
    </section></>
  )
}
