import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { bytesLabel, type DataOperation, type DataRequest } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from '../components/DataOperations'

type SourceContext = { name: string; endpoint: string; desktop: boolean; supported: boolean; message?: string }
type SourceObject = { kind: string; name: string; id: string; size?: number; supported: boolean; message?: string; composeProject?: string; composeService?: string }
type Inventory = { context: SourceContext; objects: SourceObject[]; warnings: string[] }
type Preview = { request: DataRequest; objects: SourceObject[]; conflicts: string[]; warnings: string[]; token: string; sourcePolicy: string }

export function Migration() {
  const { notifyError, notifyNotice } = useMessages()
  const [context, setContext] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [destinations, setDestinations] = useState<Record<string, string>>({})
  const [sensitive, setSensitive] = useState(false)
  const [preview, setPreview] = useState<Preview | null>(null)
  const [busy, setBusy] = useState(false)
  const contexts = usePolledResource<SourceContext[]>((signal) => apiGet('/api/docker/migration/contexts', signal), 0, [], 'migration:contexts')
  const inventory = usePolledResource<Inventory>((signal) => context
    ? apiGet(`/api/docker/migration/inventory?context=${encodeURIComponent(context)}`, signal)
    : Promise.resolve({ context: { name: '', endpoint: '', supported: false, desktop: false }, objects: [], warnings: [] }), 0, [context])
  const keyFor = (item: SourceObject) => `${item.kind}:${item.id}`
  async function prepare() {
    setBusy(true)
    try {
      const selections = (inventory.data?.objects ?? []).filter((item) => selected.has(keyFor(item)))
        .map((item) => ({ kind: item.kind, name: item.name, id: item.id, destination: destinations[keyFor(item)] || item.name }))
      setPreview(await apiSend<Preview>('/api/docker/storage/preview', 'POST', { action: 'migration', context, selections, includeSensitive: sensitive }))
    } catch (err) { notifyError('migration', errorMessage(err, 'Migration dry run failed')) }
    finally { setBusy(false) }
  }
  async function start() {
    if (!preview || preview.conflicts.length || !window.confirm(`Migrate ${preview.objects.length} selected object(s) into Porto?\n\n${preview.sourcePolicy}\nOriginal source data will not be deleted.`)) return
    setBusy(true)
    try {
      const operation = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token })
      notifyNotice('migration', `Started migration #${operation.id}. Completed objects are retained; failures are resumable.`)
      setPreview(null)
    } catch (err) { notifyError('migration', errorMessage(err, 'Unable to start migration')) }
    finally { setBusy(false) }
  }
  return <>
    <section className="fleetRail" aria-label="Runtime migration"><span className="fleetRailTitle">Local runtime migration</span><span className="fleetMessage">Source-preserving transfer · no account or credential import</span></section>
    <div className="dataWorkArea">
      <section className="drawerPanel">
        <h2>Select a source context</h2>
        <p>Docker Desktop contexts are detected alongside other local Docker sockets. Selecting one here never changes your active Docker context.</p>
        {contexts.error && <p role="alert" className="errorLine">{contexts.error}</p>}
        <label className="inspectorForm">Source Docker context<select value={context} disabled={busy} onChange={(event) => { setContext(event.target.value); setSelected(new Set()); setPreview(null) }}>
          <option value="">Choose a source</option>{contexts.data?.map((source) => <option value={source.name} key={source.name} disabled={!source.supported}>{source.name}{source.desktop ? ' · Docker Desktop' : ''}{!source.supported ? ' · unavailable' : ''}</option>)}
        </select></label>
        {contexts.data?.filter((source) => !source.supported).map((source) => <p key={source.name}>{source.name}: {source.message}</p>)}
        {inventory.error && <p role="alert" className="errorLine">{inventory.error}</p>}
        {inventory.loading && context && <p role="status">Reading source inventory without mutation…</p>}
        {inventory.data?.warnings.map((warning) => <p className="hintLine" key={warning}>{warning}</p>)}
      </section>
      <section className="drawerPanel"><h3>Choose individual objects</h3>
        <p className="hintLine">Select container dependencies (images, named volumes and custom networks) too. Unreferenced volumes require a host-accessible mountpoint or a local archive; Porto will not silently create source helper containers.</p>
        <div className="dataTableScroll"><table className="dataTable">
          <thead><tr><th>Select</th><th>Source</th><th>Destination / capability</th></tr></thead>
          <tbody>{inventory.data?.objects.map((item) => <tr key={keyFor(item)}>
            <td><input type="checkbox" aria-label={`Migrate ${item.kind} ${item.name}`} disabled={!item.supported || busy} checked={selected.has(keyFor(item))} onChange={(event) => { setSelected((current) => { const next = new Set(current); if (event.target.checked) next.add(keyFor(item)); else next.delete(keyFor(item)); return next }); setPreview(null) }} /></td>
            <td><strong>{item.name}</strong><br />{item.kind} · {bytesLabel(item.size)}<br />{item.composeProject && `${item.composeProject}/${item.composeService || ''}`}</td>
            <td>{item.kind === 'image' ? item.name : <input type="text" aria-label={`Destination for ${item.name}`} value={destinations[keyFor(item)] ?? item.name} disabled={busy} onChange={(event) => { setDestinations((current) => ({ ...current, [keyFor(item)]: event.target.value })); setPreview(null) }} />}{item.message && <p>{item.message}</p>}</td>
          </tr>)}</tbody>
        </table></div>
        <label className="toggleRow"><span>Explicitly allow sensitive container environment values through this local socket transfer (never registry credentials)</span><input type="checkbox" checked={sensitive} onChange={(event) => { setSensitive(event.target.checked); setPreview(null) }} /></label>
        <button type="button" disabled={busy || !selected.size || !context} onClick={() => void prepare()}>Dry-run selected migration</button>
      </section>
      {preview && <section className="drawerPanel" aria-label="Migration dry-run report">
        <h3>Dry-run report</h3><p>{preview.sourcePolicy}</p>
        {preview.conflicts.map((conflict) => <p role="alert" className="errorLine" key={conflict}>{conflict}</p>)}
        {preview.warnings.map((warning) => <p className="hintLine" key={warning}>{warning}</p>)}
        <p>{preview.objects.length} exact source identities selected. Original source resources are never removed.</p>
        <button type="button" disabled={busy || preview.conflicts.length > 0} onClick={() => void start()}>Confirm selected migration</button>
      </section>}
      <DataOperations />
    </div>
  </>
}
