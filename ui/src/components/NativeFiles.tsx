import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { FileResource } from '../dataTypes'

type Capability = { supported: boolean; driver: string; message: string; fallback: string; identity?: string; resource?: FileResource }
type Attachment = { id: string; identity: string; resource: { kind: string; name: string; id: string }; path: string; readOnly: boolean; state: string; message?: string }

export function NativeFiles({ kind, name }: { kind: 'container' | 'image' | 'volume' | 'vm'; name: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [writableIdentity, setWritableIdentity] = useState('')
  const [busy, setBusy] = useState(false)
  const capability = usePolledResource<Capability>((signal) => apiGet(`/api/files/capabilities?kind=${kind}&name=${encodeURIComponent(name)}`, signal), 5000, [kind, name], `files:capabilities:${kind}:${name}`)
  const attachments = usePolledResource<Attachment[]>((signal) => apiGet('/api/files/attachments', signal), 5000, [], 'files:attachments')
  const writable = Boolean(kind !== 'image' && !capability.data?.resource?.readOnly && capability.data?.identity && writableIdentity === capability.data.identity)
  const matching = (attachments.data ?? []).filter((attachment) => attachment.resource.kind === kind && (attachment.resource.name === name || attachment.resource.id === name || kind === 'image' && attachment.resource.name.endsWith(`/${name}`)))
  const outdated = (attachment: Attachment) => Boolean(capability.data?.identity && attachment.identity !== capability.data.identity)
  const needsDetach = matching.some((attachment) => attachment.state !== 'connected' || outdated(attachment))
  async function attach() {
    if (!window.confirm(`Attach ${kind} ${name} to a real host filesystem path${writable ? ' with direct write access' : ' read-only'}?\nNo copy-back is used. Detach before deleting or changing the resource.`)) return
    setBusy(true)
    try {
      const attachment = await apiSend<Attachment>('/api/files/attachments', 'POST', { kind, name, identity: capability.data?.identity, writable, confirm: true })
      attachments.reload()
      if (window.portoDesktop?.openFiles) await window.portoDesktop.openFiles(attachment.id)
      else notifyNotice('files', `Native path: ${attachment.path}`)
    } catch (err) { notifyError('files', errorMessage(err, 'Native filesystem access failed')) }
    finally { setBusy(false) }
  }
  async function open(attachment: Attachment) {
    setBusy(true)
    try {
      await apiGet(`/api/files/attachments/${encodeURIComponent(attachment.id)}`)
      if (window.portoDesktop?.openFiles) await window.portoDesktop.openFiles(attachment.id)
      else notifyNotice('files', `Open this path in your file manager: ${attachment.path}`)
    } catch (err) { notifyError('files', errorMessage(err, 'Native path is disconnected')) }
    finally { attachments.reload(); setBusy(false) }
  }
  async function detach(attachment: Attachment) {
    setBusy(true)
    try { await apiSend(`/api/files/attachments/${encodeURIComponent(attachment.id)}`, 'DELETE') }
    catch (err) { notifyError('files', errorMessage(err, 'Unable to detach native files')) }
    finally { attachments.reload(); setBusy(false) }
  }
  return <section className="drawerPanel">
    <h3>Native host files</h3>
    {(capability.error || attachments.error) && <p role="alert" className="errorLine">{capability.error || attachments.error}</p>}
    {capability.loading && !capability.data && <p role="status">Checking native file access for this resource...</p>}
    {capability.data && <><p>{capability.data.driver && `${capability.data.driver}: `}{capability.data.message}</p><p className="hintLine">{capability.data.fallback}</p></>}
    {kind !== 'image' && <label className="toggleRow"><span>Explicit direct write access (not synchronized copies)</span><input type="checkbox" checked={writable} onChange={(event) => setWritableIdentity(event.target.checked ? capability.data?.identity || '' : '')} disabled={busy || !capability.data?.supported || !capability.data.identity || capability.data.resource?.readOnly} /></label>}
    {kind === 'image' && <p>Immutable images are always mounted read-only; writes are rejected.</p>}
    {needsDetach && <p className="hintLine">Detach disconnected or outdated attachments before reconnecting. Source data is not removed.</p>}
    <button type="button" disabled={busy || capability.loading || !capability.data?.supported || !capability.data.identity || needsDetach} onClick={() => void attach()}>Open files in native file manager</button>
    {matching.map((attachment) => <div key={attachment.id}>
      <p><code>{attachment.path}</code> · {attachment.readOnly ? 'read-only' : 'direct writes'} · {attachment.state}</p>
      {outdated(attachment) && <p role="alert" className="errorLine">Resource identity changed. Detach this attachment before opening the replacement.</p>}
      {attachment.message && <p className="hintLine">{attachment.message}</p>}
      <div className="dataActions"><button type="button" disabled={busy || attachment.state !== 'connected' || outdated(attachment)} onClick={() => void open(attachment)}>Open attached files</button><button type="button" disabled={busy} onClick={() => void detach(attachment)}>Detach files</button></div>
    </div>)}
  </section>
}
