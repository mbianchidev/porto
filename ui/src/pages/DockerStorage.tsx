import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { bytesLabel, type DataOperation, type PrunePreview, type StorageResource, type StorageUsage } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from '../components/DataOperations'

const keyFor = (item: StorageResource) => `${item.resource.kind}:${item.resource.name}:${item.resource.id}`

export function DockerStorage() {
  const { notifyError, notifyNotice } = useMessages()
  const [kind, setKind] = useState('all')
  const [filter, setFilter] = useState('all')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [preview, setPreview] = useState<PrunePreview | null>(null)
  const [busy, setBusy] = useState(false)
  const usage = usePolledResource<StorageUsage>((signal) => apiGet('/api/docker/storage', signal), 30000, [], 'docker:storage')
  const resources = usage.data?.resources ?? []
  const rows = resources.filter((item) => (kind === 'all' || item.resource.kind === kind) && (filter === 'all' || filter === 'in-use' && item.inUse || filter === 'unused' && !item.inUse || filter === 'dangling' && item.dangling))
  async function prepare() {
    setBusy(true)
    try {
      const selections = resources.filter((item) => selected.has(keyFor(item))).map((item) => ({ kind: item.resource.kind, name: item.resource.name, id: item.resource.id }))
      setPreview(await apiSend<PrunePreview>('/api/docker/storage/preview', 'POST', { action: 'prune', selections }))
    } catch (err) { notifyError('storage', errorMessage(err, 'Unable to preview scoped cleanup')) }
    finally { setBusy(false) }
  }
  async function prune() {
    if (!preview || !window.confirm(`Remove exactly ${preview.candidates.length} previewed unused resource(s)? Referenced and managed-cluster resources are excluded.`)) return
    setBusy(true)
    try {
      const run = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token })
      notifyNotice('storage', `Started scoped cleanup #${run.id}.`)
      setPreview(null)
      setSelected(new Set())
      usage.reload()
    } catch (err) { notifyError('storage', errorMessage(err, 'Unable to prune selected resources')) }
    finally { setBusy(false) }
  }
  return <>
    <section className="fleetRail" aria-label="Storage accounting"><span className="fleetRailTitle">Docker storage</span><span className="fleetDatum">Unique content {bytesLabel(usage.data?.contentBytes)}</span><span className="fleetDatum">Snapshots {bytesLabel(usage.data?.snapshotBytes)}</span><span className="fleetDatum">Volumes {bytesLabel(usage.data?.volumeBytes)}</span></section>
    <div className="controlBar">
      <label>Resource type <select value={kind} onChange={(event) => setKind(event.target.value)}>{['all', 'image', 'container', 'volume', 'network', 'cache'].map((value) => <option key={value}>{value}</option>)}</select></label>
      <label>Reference filter <select value={filter} onChange={(event) => setFilter(event.target.value)}>{['all', 'in-use', 'unused', 'dangling'].map((value) => <option key={value}>{value}</option>)}</select></label>
      <button type="button" onClick={usage.reload}>Refresh storage</button>
      <button type="button" disabled={busy || selected.size === 0} onClick={() => void prepare()}>Preview selected cleanup</button>
    </div>
    <div className="dataWorkArea">
      {usage.error && <p role="alert" className="errorLine">{usage.error}</p>}
      {usage.loading && !usage.data && <p role="status">Measuring stores and resource ownership…</p>}
      <section className="drawerPanel"><h2>Allocation and ownership</h2>
        <p className="hintLine">{usage.data?.accounting}</p>
        {usage.data?.warnings.map((warning) => <p key={warning} className="hintLine">{warning}</p>)}
        <dl className="runtimeGrid"><div><dt>Writable snapshots</dt><dd>{bytesLabel(usage.data?.writableBytes)}</dd></div><div><dt>Non-shared build cache</dt><dd>{bytesLabel(usage.data?.buildCacheBytes)}</dd></div><div><dt>Shared runtime metadata</dt><dd>{bytesLabel(usage.data?.metadataBytes)}</dd></div></dl>
        <div className="dataTableScroll"><table className="dataTable">
          <thead><tr><th>Select</th><th>Resource</th><th>Logical / allocated / shared</th><th>References and protections</th></tr></thead>
          <tbody>{rows.map((item) => <tr key={keyFor(item)}>
            <td><input type="checkbox" aria-label={`Select ${item.resource.kind} ${item.resource.name}`} disabled={item.inUse || item.protected || busy || Boolean(usage.error)} checked={selected.has(keyFor(item))} onChange={(event) => { setSelected((current) => { const next = new Set(current); if (event.target.checked) next.add(keyFor(item)); else next.delete(keyFor(item)); return next }); setPreview(null) }} /></td>
            <td><strong>{item.resource.name}</strong><br />{item.resource.kind}{item.dangling ? ' · dangling' : ''}</td>
            <td>{bytesLabel(item.logicalBytes)} / {bytesLabel(item.allocatedBytes)} / {bytesLabel(item.sharedBytes)}</td>
            <td>{item.owners.map((owner) => `${owner.name} (${owner.state}${owner.composeProject ? ` · ${owner.composeProject}/${owner.composeService || ''}` : ''})`).join(', ') || 'No container references'}{item.reason && <p>{item.reason}</p>}</td>
          </tr>)}</tbody>
        </table></div>
      </section>
      {preview && <section className="drawerPanel" aria-label="Scoped cleanup preview">
        <h3>Exact removal preview</h3><p>{preview.message}</p><p>Measured allocation upper bound: {bytesLabel(preview.upperBoundBytes)}</p>
        <ul>{preview.candidates.map((item) => <li key={keyFor(item)}>{item.resource.kind}: {item.resource.name} · identity {item.identity.slice(0, 12)}</li>)}</ul>
        <p>{preview.excluded.length} referenced/protected object(s) excluded.</p>
        <button type="button" className="destructiveAction" disabled={busy || !preview.candidates.length} onClick={() => void prune()}>Confirm exact cleanup</button>
      </section>}
      <DataOperations />
    </div>
  </>
}
