import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage, isAbortError } from '../api'
import { bytesLabel, type DataOperation, type PrunePreview, type StorageResource, type StorageUsage } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from '../components/DataOperations'
import { Inspector } from '../components/Inspector'
import { VolumeTools } from '../components/VolumeTools'

const resourceKinds = ['image', 'container', 'volume', 'network', 'cache']
const kindLabels: Record<string, string> = { image: 'Images', container: 'Writable containers', volume: 'Volumes', network: 'Networks', cache: 'Build cache' }
const keyFor = (item: StorageResource) => item.identity

export function DockerStorage() {
  const { notifyError, notifyNotice } = useMessages()
  const [kind, setKind] = useState('all')
  const [filter, setFilter] = useState('all')
  const [query, setQuery] = useState('')
  const [category, setCategory] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [preview, setPreview] = useState<PrunePreview | null>(null)
  const [volumeName, setVolumeName] = useState('')
  const [busy, setBusy] = useState(false)
  const controller = useRef<AbortController | null>(null)
  useEffect(() => () => controller.current?.abort(), [])
  const usage = usePolledResource<StorageUsage>((signal) => apiGet('/api/docker/storage', signal), 30000, [], 'docker:storage')
  const resources = usage.data?.resources ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const rows = resources.filter((item) =>
    (kind === 'all' || item.resource.kind === kind) &&
    (filter === 'all' || filter === 'in-use' && item.inUse || filter === 'unused' && !item.inUse || filter === 'dangling' && item.dangling) &&
    (!normalizedQuery || [item.resource.name, item.description, ...item.owners.flatMap((owner) => [owner.name, owner.composeProject, owner.composeService])]
      .some((value) => value?.toLocaleLowerCase().includes(normalizedQuery))),
  )
  const selections = resources.filter((item) => selected.has(keyFor(item)) && !item.inUse && !item.protected)
  const canConfirm = Boolean(preview?.candidates.length && !usage.error && preview.candidates.every((item) =>
    resources.some((current) => current.identity === item.identity && !current.inUse && !current.protected)))

  function changeFilter(update: () => void) {
    update()
    setSelected(new Set())
    setPreview(null)
  }
  async function prepare(categoryScoped: boolean) {
    controller.current?.abort()
    const current = new AbortController()
    controller.current = current
    setBusy(true)
    setPreview(null)
    try {
      const request = categoryScoped
        ? { action: 'prune', categories: [category] }
        : { action: 'prune', selections: selections.map((item) => ({ kind: item.resource.kind, name: item.resource.name, id: item.resource.id })) }
      setPreview(await apiSend<PrunePreview>('/api/docker/storage/preview', 'POST', request, current.signal))
    } catch (err) {
      if (!isAbortError(err)) notifyError('storage', errorMessage(err, 'Unable to preview scoped cleanup'))
    } finally {
      if (controller.current === current) setBusy(false)
    }
  }
  async function prune() {
    if (!preview || !canConfirm || !window.confirm(`Remove exactly ${preview.candidates.length} previewed unused resource(s)? Referenced and managed-cluster resources are excluded. Actual reclaimed bytes are not guaranteed.`)) return
    setBusy(true)
    try {
      const run = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token })
      notifyNotice('storage', `Started scoped cleanup #${run.id}. Track progress and cancellation below or in Activity.`)
      setPreview(null)
      setSelected(new Set())
    } catch (err) { notifyError('storage', errorMessage(err, 'Unable to prune selected resources')) }
    finally { setBusy(false) }
  }

  return <>
    <section className="fleetRail" aria-label="Storage accounting">
      <span className="fleetRailTitle">Docker storage</span>
      <span className="fleetDatum">Unique content {bytesLabel(usage.data?.contentBytes)}</span>
      <span className="fleetDatum">Snapshots {bytesLabel(usage.data?.snapshotBytes)}</span>
      <span className="fleetDatum">Volumes {bytesLabel(usage.data?.volumeBytes)}</span>
    </section>
    <div className="controlBar storageControls">
      <label className="projectSearch"><span className="visuallyHidden">Search storage resources and owners</span><input type="search" placeholder="Resource or Compose application" value={query} onChange={(event) => changeFilter(() => setQuery(event.target.value))} /></label>
      <label>Resource type <select value={kind} onChange={(event) => changeFilter(() => setKind(event.target.value))}><option value="all">All resources</option>{resourceKinds.map((value) => <option key={value} value={value}>{kindLabels[value]}</option>)}</select></label>
      <label>Reference filter <select value={filter} onChange={(event) => changeFilter(() => setFilter(event.target.value))}><option value="all">All references</option><option value="in-use">In use</option><option value="unused">Unused</option><option value="dangling">Dangling</option></select></label>
      <button type="button" onClick={() => { setPreview(null); usage.reload() }}>Refresh storage</button>
    </div>
    <div className="workArea storageWorkArea">
      <div className="dataWorkArea">
        {usage.error && <p role="alert" className="errorLine">{usage.error}</p>}
        {usage.loading && !usage.data && <p role="status">Measuring stores and resource ownership...</p>}
        <section className="drawerPanel" aria-label="Storage allocation">
          <h3>Allocation and ownership</h3>
          <p className="hintLine">{usage.data?.accounting}</p>
          <dl className="runtimeGrid">
            <div><dt>Writable snapshots (included above)</dt><dd>{bytesLabel(usage.data?.writableBytes)}</dd></div>
            <div><dt>Non-shared build cache</dt><dd>{bytesLabel(usage.data?.buildCacheBytes)}</dd></div>
            <div><dt>Runtime metadata (all namespaces)</dt><dd>{bytesLabel(usage.data?.metadataBytes)}</dd></div>
          </dl>
          {usage.data?.warnings.map((warning) => <p key={warning} className="hintLine">{warning}</p>)}
          <p className="filterResultCount" aria-live="polite">{rows.length} / {resources.length} resources; {selections.length} selected for cleanup</p>
          {!usage.loading && !rows.length && <p>No resources match these filters.</p>}
          <div className="dataTableScroll"><table className="dataTable" aria-label="Storage resources">
            <thead><tr><th>Select</th><th>Resource</th><th>Logical / allocated / shared</th><th>References and protections</th></tr></thead>
            <tbody>{rows.map((item) => <tr key={keyFor(item)}>
              <td><input type="checkbox" aria-label={`Select ${item.resource.kind} ${item.resource.name}`} disabled={item.inUse || item.protected || busy || Boolean(usage.error)} checked={selected.has(keyFor(item))} onChange={(event) => {
                setSelected((current) => { const next = new Set(current); if (event.target.checked) next.add(keyFor(item)); else next.delete(keyFor(item)); return next })
                setPreview(null)
              }} /></td>
              <td><strong>{item.resource.name}</strong><br />{kindLabels[item.resource.kind] || item.resource.kind}{item.dangling ? ' / dangling' : ''}
                {item.description && <p>{item.description}</p>}
                {item.cacheType && <p>{item.cacheType} / used {item.usageCount ?? 0} time(s){item.lastUsedAt ? ` / last used ${new Date(item.lastUsedAt).toLocaleString()}` : ''}</p>}
                {item.resource.kind === 'volume' && <button type="button" onClick={() => setVolumeName(item.resource.name)}>Manage volume {item.resource.name}</button>}
              </td>
              <td>{bytesLabel(item.logicalBytes)} / {bytesLabel(item.allocatedBytes)} / {bytesLabel(item.sharedBytes)}</td>
              <td>{item.owners.map((owner) => `${owner.name} (${owner.state}${owner.composeProject ? ` / ${owner.composeProject}/${owner.composeService || ''}` : ''}${owner.managed ? ' / managed Kubernetes' : ''})`).join(', ') || (item.inUse ? 'Backend reports this resource in use' : 'No container references')}{item.reason && <p>{item.reason}</p>}</td>
            </tr>)}</tbody>
          </table></div>
        </section>
        <section className="drawerPanel" aria-label="Scoped cleanup controls">
          <h3>Scoped cleanup</h3>
          <p className="hintLine">Select individual objects above or choose one explicit category. No broad cleanup is selected by default.</p>
          <div className="inspectorForm inline">
            <button type="button" disabled={busy || !selections.length || Boolean(usage.error)} onClick={() => void prepare(false)}>Preview selected cleanup</button>
            <label>Cleanup category<select value={category} disabled={busy} onChange={(event) => { setCategory(event.target.value); setPreview(null) }}><option value="">Choose a category</option>{resourceKinds.map((value) => <option key={value} value={value}>{kindLabels[value]}</option>)}</select></label>
            <button type="button" disabled={busy || !category || Boolean(usage.error) || !usage.data} onClick={() => void prepare(true)}>Preview category cleanup</button>
            {busy && <button type="button" onClick={() => { controller.current?.abort(); setBusy(false) }}>Cancel preparation</button>}
          </div>
        </section>
        {preview && <section className="drawerPanel" aria-label="Scoped cleanup preview">
          <h3>Exact removal preview</h3><p>{preview.message}</p><p>Measured allocation upper bound: {bytesLabel(preview.upperBoundBytes)}. This is not guaranteed reclaimed space.</p>
          <ul>{preview.candidates.map((item) => <li key={keyFor(item)}>{item.resource.kind}: {item.resource.name} / identity {item.identity.slice(0, 12)}</li>)}</ul>
          {preview.excluded.map((item) => <p key={keyFor(item)}>{item.resource.name} excluded: {item.reason || 'Referenced by a container or active build.'}</p>)}
          {!canConfirm && preview.candidates.length > 0 && <p role="alert" className="errorLine">Resource ownership changed or could not be refreshed. Preview again before cleanup.</p>}
          <button type="button" className="destructiveAction" disabled={busy || !canConfirm} onClick={() => void prune()}>Confirm exact cleanup</button>
        </section>}
        <DataOperations onComplete={usage.reload} />
      </div>
      {volumeName && <Inspector title={volumeName} subtitle="Volume data and local backups" onClose={() => setVolumeName('')}><VolumeTools key={volumeName} name={volumeName} /></Inspector>}
    </div>
  </>
}
