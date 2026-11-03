import { useEffect, useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage, isAbortError } from '../api'
import { bytesLabel, type DataOperation, type PrunePreview, type StorageResource, type StorageUsage } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { DataOperations } from '../components/DataOperations'
import { Inspector } from '../components/Inspector'
import { VolumeTools } from '../components/VolumeTools'
import { ResourceInput } from '../components/ResourceInput'

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
  const [previewScope, setPreviewScope] = useState('')
  const [volumeName, setVolumeName] = useState('')
  const [activity, setActivity] = useState<'preparing' | 'starting' | null>(null)
  const [failure, setFailure] = useState('')
  const controller = useRef<AbortController | null>(null)
  const volumeTrigger = useRef<HTMLButtonElement | null>(null)
  useEffect(() => () => controller.current?.abort(), [])
  const usage = usePolledResource<StorageUsage>((signal) => apiGet('/api/docker/storage', signal), 30000, [], 'docker:storage')
  const resources = usage.data?.resources ?? []
  const busy = activity !== null
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const hasFilters = kind !== 'all' || filter !== 'all' || query !== ''
  const rows = resources.filter((item) =>
    (kind === 'all' || item.resource.kind === kind) &&
    (filter === 'all' || filter === 'in-use' && item.inUse || filter === 'unused' && !item.inUse || filter === 'dangling' && item.dangling) &&
    (!normalizedQuery || [item.resource.name, item.description, ...item.owners.flatMap((owner) => [owner.name, owner.composeProject, owner.composeService])]
      .some((value) => value?.toLocaleLowerCase().includes(normalizedQuery))),
  )
  const selections = resources.filter((item) => selected.has(keyFor(item)) && !item.inUse && !item.protected)
  const canConfirm = Boolean(preview?.candidates.length && !usage.error && preview.candidates.every((item) =>
    resources.some((current) => current.identity === item.identity && !current.inUse && !current.protected)))

  function clearPreview() {
    controller.current?.abort()
    controller.current = null
    setActivity((current) => current === 'preparing' ? null : current)
    setPreview(null)
    setPreviewScope('')
    setFailure('')
  }
  function changeFilter(update: () => void) {
    clearPreview()
    update()
    setSelected(new Set())
  }
  function closeVolume() {
    setVolumeName('')
    if (volumeTrigger.current?.isConnected) volumeTrigger.current.focus()
    else document.getElementById('storage-search')?.focus()
  }
  async function prepare(categoryScoped: boolean) {
    clearPreview()
    const current = new AbortController()
    controller.current = current
    setActivity('preparing')
    try {
      const request = categoryScoped
        ? { action: 'prune', categories: [category] }
        : { action: 'prune', selections: selections.map((item) => ({ kind: item.resource.kind, name: item.resource.name, id: item.resource.id })) }
      const result = await apiSend<PrunePreview>('/api/docker/storage/preview', 'POST', request, current.signal)
      if (controller.current !== current || current.signal.aborted) return
      setPreview(result)
      setPreviewScope(categoryScoped ? `Category: ${kindLabels[category]}` : `Selected resources: ${selections.length}`)
    } catch (err) {
      if (!isAbortError(err) && controller.current === current) {
        const message = errorMessage(err, 'Unable to preview scoped cleanup')
        setFailure(message)
        notifyError('storage', message)
      }
    } finally {
      if (controller.current === current) {
        controller.current = null
        setActivity(null)
      }
    }
  }
  async function prune() {
    if (!preview || !canConfirm || !window.confirm(`Remove exactly ${preview.candidates.length} previewed unused resource(s)? Referenced and managed-cluster resources are excluded. Actual reclaimed bytes are not guaranteed.`)) return
    setActivity('starting')
    setFailure('')
    try {
      const run = await apiSend<DataOperation>('/api/data/operations', 'POST', { ...preview.request, confirm: true, preview: preview.token })
      notifyNotice('storage', `Started scoped cleanup #${run.id}. Track progress and cancellation below or in Activity.`)
      setPreview(null)
      setSelected(new Set())
    } catch (err) {
      const message = errorMessage(err, 'Unable to prune selected resources')
      setFailure(message)
      notifyError('storage', message)
    } finally { setActivity(null) }
  }

  return <>
    <section className="fleetRail storageSummary" aria-label="Storage accounting">
      <span className="fleetRailTitle">Docker storage</span>
      <dl className="storageSummaryMetrics">
        <div><dt>Unique content</dt><dd>{bytesLabel(usage.data?.contentBytes)}</dd></div>
        <div><dt>Snapshots</dt><dd>{bytesLabel(usage.data?.snapshotBytes)}</dd></div>
        <div><dt>Volumes</dt><dd>{bytesLabel(usage.data?.volumeBytes)}</dd></div>
      </dl>
    </section>
    <div className="controlBar storageControls">
      <div className="storageField storageSearch">
        <label htmlFor="storage-search">Search storage resources and owners</label>
        <ResourceInput id="storage-search" options={resources.filter((item) => kind === 'all' || item.resource.kind === kind).map((item) => item.resource.name)} type="search" placeholder="Resource, container or Compose application" value={query} onChange={(event) => changeFilter(() => setQuery(event.target.value))} />
      </div>
      <div className="storageField">
        <label htmlFor="storage-kind">Resource type</label>
        <select id="storage-kind" value={kind} onChange={(event) => changeFilter(() => setKind(event.target.value))}><option value="all">All resources</option>{resourceKinds.map((value) => <option key={value} value={value}>{kindLabels[value]}</option>)}</select>
      </div>
      <div className="storageField">
        <label htmlFor="storage-filter">Reference filter</label>
        <select id="storage-filter" value={filter} onChange={(event) => changeFilter(() => setFilter(event.target.value))}><option value="all">All reference states</option><option value="in-use">In use</option><option value="unused">Unused (may be protected)</option><option value="dangling">Dangling</option></select>
      </div>
      <button type="button" onClick={() => { clearPreview(); usage.reload() }}>Refresh storage</button>
      {hasFilters && <button type="button" onClick={() => changeFilter(() => { setKind('all'); setFilter('all'); setQuery('') })}>Clear filters</button>}
    </div>
    <div className="workArea storageWorkArea">
      <div className="dataWorkArea">
        {usage.error && <p role="alert" className="errorBanner">{usage.error} Cleanup is disabled until storage can be refreshed.</p>}
        {usage.loading && !usage.data && <p role="status">Measuring stores and resource ownership...</p>}
        <section className="drawerPanel storageAccounting" aria-label="Storage allocation">
          <h3>Storage measurements</h3>
          <p className="hintLine">{usage.data?.accounting}</p>
          <dl className="runtimeGrid">
            <div><dt>Writable snapshots</dt><dd>{bytesLabel(usage.data?.writableBytes)}</dd></div>
            <div><dt>Non-shared build cache</dt><dd>{bytesLabel(usage.data?.buildCacheBytes)}</dd></div>
            <div><dt>Runtime metadata (all namespaces)</dt><dd>{bytesLabel(usage.data?.metadataBytes)}</dd></div>
          </dl>
          <p className="hintLine">Writable snapshots are included in snapshots.</p>
          {usage.data?.warnings.map((warning) => <p key={warning} className="hintLine">{warning}</p>)}
        </section>
        <section className="drawerPanel" aria-label="Storage resources">
          <div className="drawerPanelHeading">
            <h3>Storage resources</h3>
            {usage.data && <p className="filterResultCount" aria-live="polite">{rows.length} of {resources.length} resources</p>}
          </div>
          <p id="storage-size-help" className="hintLine">Logical is the reported data size; allocated is measured storage; shared overlaps other resources. Values may overlap and must not be added together. Unavailable is not zero.</p>
          <p className="hintLine">Scroll the table horizontally for more columns when space is limited.</p>
          {!usage.loading && !usage.error && usage.data && !rows.length && <div className="storageEmpty" role="status">
            <p>{resources.length ? 'No resources match these filters.' : 'No storage resources reported.'}</p>
            <p className="hintLine">{resources.length ? 'Clear the filters to see all resources.' : 'Refresh after creating or importing a resource.'}</p>
          </div>}
          {rows.length > 0 && <div className="dataTableScroll storageResourceScroll" role="region" aria-label="Scrollable storage resources" aria-describedby="storage-size-help" tabIndex={0}><table className="dataTable storageTable" aria-label="Storage resources">
            <colgroup><col className="storageSelectColumn" /><col className="storageResourceColumn" /><col span={3} className="storageSizeColumn" /><col /></colgroup>
            <thead><tr><th scope="col"><span className="visuallyHidden">Select</span></th><th scope="col">Resource</th><th scope="col">Logical</th><th scope="col">Allocated</th><th scope="col">Shared</th><th scope="col">References and protections</th></tr></thead>
            <tbody>{rows.map((item, index) => <tr key={keyFor(item)} className={selected.has(keyFor(item)) && !item.inUse && !item.protected ? 'storageRowSelected' : undefined}>
              <td><input type="checkbox" aria-label={`Select ${item.resource.kind} ${item.resource.name}`} aria-describedby={`storage-protection-${index}`} disabled={item.inUse || item.protected || busy || Boolean(usage.error)} checked={selected.has(keyFor(item)) && !item.inUse && !item.protected} onChange={(event) => {
                setSelected((current) => { const next = new Set(current); if (event.target.checked) next.add(keyFor(item)); else next.delete(keyFor(item)); return next })
                clearPreview()
              }} /></td>
              <td><strong className="storageResourceName">{item.resource.name}</strong><span className="storageResourceKind">{kindLabels[item.resource.kind] || item.resource.kind}{item.dangling ? ' / dangling' : ''}</span>
                {item.description && <p>{item.description}</p>}
                {item.cacheType && <p>{item.cacheType} / used {item.usageCount ?? 0} time(s){item.lastUsedAt ? ` / last used ${new Date(item.lastUsedAt).toLocaleString()}` : ''}</p>}
                {item.resource.kind === 'volume' && <button type="button" className="storageManageVolume" aria-label={`Manage volume ${item.resource.name}`} onClick={(event) => { volumeTrigger.current = event.currentTarget; setVolumeName(item.resource.name) }}>Manage volume</button>}
              </td>
              <td className="storageSize">{bytesLabel(item.logicalBytes)}</td>
              <td className="storageSize">{bytesLabel(item.allocatedBytes)}</td>
              <td className="storageSize">{bytesLabel(item.sharedBytes)}</td>
              <td>
                <p id={`storage-protection-${index}`} className={`storageProtection${item.protected ? ' protected' : ''}`}><strong>{item.protected ? 'Protected' : item.inUse ? 'In use' : 'Unused'}</strong>{item.protected && item.inUse ? ' / in use' : ''}{item.reason && <span>{item.reason}</span>}</p>
                {item.owners.length ? <ul className="storageOwners">{item.owners.map((owner) => <li key={owner.id}>
                  <strong>{owner.name}</strong> ({owner.state})
                  {owner.composeProject && <span>{owner.composeProject}/{owner.composeService || ''}</span>}
                  {owner.managed && <span>Managed Kubernetes</span>}
                </li>)}</ul> : <p>{item.inUse ? 'Backend reports this resource in use' : 'No container references'}</p>}
              </td>
            </tr>)}</tbody>
          </table></div>}
        </section>
        <section className="drawerPanel storageCleanup" aria-label="Scoped cleanup controls">
          <h3>Scoped cleanup</h3>
          <p className="hintLine">Preview first, then explicitly confirm the exact resources to remove. In-use and protected resources cannot be selected.</p>
          <div className="storageCleanupScopes">
            <fieldset>
              <legend>Selected resources</legend>
              <p aria-live="polite">{selections.length} selected for cleanup</p>
              <p className="hintLine">Only the checked resources above are included. Changing a resource filter clears the selection.</p>
              <div className="dataActions">
                <button type="button" disabled={busy || !selections.length || Boolean(usage.error)} onClick={() => void prepare(false)}>Preview selected cleanup</button>
                <button type="button" disabled={busy || !selected.size} onClick={() => { clearPreview(); setSelected(new Set()) }}>Clear selection</button>
              </div>
            </fieldset>
            <fieldset>
              <legend>Category cleanup</legend>
              <p className="hintLine">Checks one entire category and ignores the resource filters and selection. No category is selected by default.</p>
              <div className="storageField">
                <label htmlFor="storage-category">Cleanup category</label>
                <select id="storage-category" value={category} disabled={busy} onChange={(event) => { clearPreview(); setCategory(event.target.value) }}><option value="">Choose a category</option>{resourceKinds.map((value) => <option key={value} value={value}>{kindLabels[value]}</option>)}</select>
              </div>
              <div className="dataActions"><button type="button" disabled={busy || !category || Boolean(usage.error) || !usage.data} onClick={() => void prepare(true)}>Preview category cleanup</button></div>
            </fieldset>
          </div>
          {activity === 'preparing' && <div className="dataActions"><p role="status">Preparing exact cleanup preview...</p><button type="button" onClick={clearPreview}>Cancel preparation</button></div>}
          {activity === 'starting' && <p role="status">Starting cleanup. Track progress and cancellation in data operation history.</p>}
          {failure && <p role="alert" className="errorLine">{failure}</p>}
        </section>
        {preview && <section className="drawerPanel storageCleanupPreview" aria-label="Scoped cleanup preview">
          <h3>Exact removal preview</h3>
          <p className="storagePreviewScope">{previewScope}</p>
          <p>{preview.message}</p>
          <p>Measured allocation upper bound: {bytesLabel(preview.upperBoundBytes)}. This is not guaranteed reclaimed space.</p>
          <p role="status">{preview.candidates.length ? `${preview.candidates.length} unused resource(s) will be removed only after confirmation.` : 'No eligible unused resources in this scope. Nothing will be removed.'}</p>
          <ul>{preview.candidates.map((item) => <li key={keyFor(item)}>{item.resource.kind}: {item.resource.name} / identity {item.identity.slice(0, 12)}</li>)}</ul>
          {preview.excluded.map((item) => <p key={keyFor(item)}>{item.resource.name} excluded: {item.reason || 'Referenced by a container or active build.'}</p>)}
          {!canConfirm && preview.candidates.length > 0 && <p role="alert" className="errorLine">Resource ownership changed or could not be refreshed. Preview again before cleanup.</p>}
          <button type="button" className="destructiveAction" disabled={busy || !canConfirm} onClick={() => void prune()}>{activity === 'starting' ? 'Starting cleanup...' : 'Confirm exact cleanup'}</button>
        </section>}
        <DataOperations onComplete={usage.reload} />
      </div>
      {volumeName && <Inspector title={volumeName} subtitle="Volume data and local backups" onClose={closeVolume}><VolumeTools key={volumeName} name={volumeName} /></Inspector>}
    </div>
  </>
}
