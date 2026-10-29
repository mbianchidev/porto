import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useMessages } from '../useMessages'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { LocalOperations } from '../components/LocalOperations'
import { RuntimeGate } from '../components/SectionChrome'
import { StatusLamp } from '../components/StatusLamp'
import { ADMIN_TARGET_KEY, adminQuery, resourceActions, resourceKey } from '../kubeAdminTypes'
import type { AdminAccess, AdminDiscovery, AdminObject, AdminObjectList, AdminPreview, AdminSchema, AdminTarget, RolloutRevision } from '../kubeAdminTypes'
import type { KubernetesContext } from '../types'

function transferredTarget(): { target: AdminTarget | null; error: string } {
  try {
    const stored = window.sessionStorage.getItem(ADMIN_TARGET_KEY)
    if (!stored) return { target: null, error: '' }
    window.sessionStorage.removeItem(ADMIN_TARGET_KEY)
    const value: unknown = JSON.parse(stored)
    if (!value || typeof value !== 'object') throw new Error('Invalid resource selection.')
    for (const key of ['context', 'namespace', 'group', 'version', 'resource', 'name']) {
      if (!(key in value) || typeof value[key as keyof typeof value] !== 'string') throw new Error('Invalid explicit resource selection.')
    }
    return { target: value as AdminTarget, error: '' }
  } catch { return { target: null, error: 'The transferred resource selection could not be read. Select its context, namespace and served API explicitly.' } }
}

export function KubernetesAdmin({ context, contexts, onContextChange }: { context: string; contexts: KubernetesContext[]; onContextChange: (context: string) => void }) {
  const { notifyError, notifyNotice } = useMessages()
  const [incoming] = useState(transferredTarget)
  const initial = incoming.target?.context === context ? incoming.target : null
  const [namespace, setNamespace] = useState(initial?.namespace ?? 'default')
  const [chosenResource, setChosenResource] = useState(initial ? `${initial.group}/${initial.version}/${initial.resource}` : 'apps/v1/deployments')
  const [selectedName, setSelectedName] = useState(initial?.name ?? '')
  const [nameDraft, setNameDraft] = useState(initial?.name ?? '')
  const [continuation, setContinuation] = useState('')
  const [open, setOpen] = useState(Boolean(initial?.name))
  const [manifest, setManifest] = useState('')
  const [mode, setMode] = useState('apply')
  const [secretConsent, setSecretConsent] = useState(false)
  const [revealed, setRevealed] = useState<AdminObject | null>(null)
  const [preview, setPreview] = useState<AdminPreview | null>(null)
  const [showSchema, setShowSchema] = useState(false)
  const [action, setAction] = useState('scale')
  const [replicas, setReplicas] = useState(1)
  const [revision, setRevision] = useState(1)
  const [acknowledgeStorage, setAcknowledgeStorage] = useState(false)
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const inspectorTrigger = useRef<HTMLElement | null>(null)
  const status = useKubernetesStatus(context)
  const available = !status.loading && !status.error && Boolean(status.data?.available)
  const discovery = usePolledResource<AdminDiscovery>((signal) => context ? apiGet(`/api/kubernetes/admin/discovery?context=${encodeURIComponent(context)}`, signal) : Promise.resolve({ resources: [], partial: false }), 0, [context])
  const kinds = discovery.data?.resources ?? []
  const resource = kinds.find((entry) => resourceKey(entry) === chosenResource) ?? kinds[0]
  const scope = resource?.namespaced ? namespace : ''
  const ready = available && Boolean(resource) && (!resource?.namespaced || scope.trim() !== '')
  const target: AdminTarget = { context, namespace: scope, group: resource?.group ?? '', version: resource?.version ?? '', resource: resource?.resource ?? '', name: selectedName }
  const query = adminQuery(target)
  const listing = usePolledResource<AdminObjectList | null>((signal) => ready ? apiGet(`/api/kubernetes/admin/resources?${query}&continue=${encodeURIComponent(continuation)}`, signal) : Promise.resolve(null), 6000, [ready, query, continuation])
  const detail = usePolledResource<AdminObject | null>((signal) => ready && selectedName ? apiGet(`/api/kubernetes/admin/resource?${query}`, signal) : Promise.resolve(null), 5000, [ready, query])
  const access = usePolledResource<AdminAccess | null>((signal) => ready ? apiGet(`/api/kubernetes/admin/access?${query}`, signal) : Promise.resolve(null), 10000, [ready, query])
  const schema = usePolledResource<AdminSchema | null>((signal) => ready && showSchema ? apiGet(`/api/kubernetes/admin/schema?${query}`, signal) : Promise.resolve(null), 0, [ready, query, showSchema])
  const actions = resourceActions(resource)
  const selectedAction = actions.includes(action) ? action : actions[0] ?? 'delete'
  const history = usePolledResource<RolloutRevision[]>((signal) => ready && selectedName && selectedAction === 'rollback' ? apiGet(`/api/kubernetes/admin/history?${query}`, signal) : Promise.resolve([]), 0, [ready, query, selectedAction])
  const selected = detail.data
  const permitted = (verb: string) => Boolean(access.data?.verbs[verb]?.allowed)
  const readOnly = !['create', 'patch', 'update', 'delete'].some(permitted)
  const actionPermission = selectedAction === 'delete' ? 'delete' : selectedAction === 'scale' && resource?.group === 'apps' ? 'scale' : 'patch'
  const storageAction = selectedAction === 'delete' && ['persistentvolumes', 'persistentvolumeclaims', 'namespaces', 'customresourcedefinitions'].includes(resource?.resource ?? '')

  async function run(task: () => Promise<void>) {
    setBusy(true)
    try { await task() } catch (error) { notifyError('kubernetes', errorMessage(error, 'Kubernetes administration failed')) } finally { setBusy(false) }
  }

  function reset() {
    setSelectedName(''); setNameDraft(''); setContinuation(''); setManifest(''); setRevealed(null); setPreview(null); setSecretConsent(false); setOpen(false)
  }

  function select(object: AdminObject) {
    inspectorTrigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectedName(object.target.name); setNameDraft(object.target.name); setOpen(true); setManifest(''); setRevealed(null); setPreview(null); setSecretConsent(false)
  }

  function closeInspector() {
    setOpen(false); setManifest(''); setRevealed(null); setPreview(null); setSecretConsent(false)
    requestAnimationFrame(() => { if (inspectorTrigger.current?.isConnected) inspectorTrigger.current.focus() })
  }

  function createManifest() {
    inspectorTrigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectedName(''); setNameDraft(''); setMode('create'); setPreview(null); setRevealed(null); setSecretConsent(false)
    setManifest(JSON.stringify({ apiVersion: resource?.group ? `${resource.group}/${resource.version}` : resource?.version, kind: resource?.kind, metadata: { name: 'new-resource', ...(resource?.namespaced ? { namespace: scope } : {}) } }, null, 2))
    setOpen(true)
  }

  function previewManifest() {
    void run(async () => {
      const result = await apiSend<AdminPreview>('/api/kubernetes/admin/previews', 'POST', { context, namespace: scope, manifest, mode, secretConsent })
      setPreview(result)
      if (result.differences.some((difference) => difference.kind === 'Secret')) { setManifest(''); setRevealed(null); setSecretConsent(false) }
    })
  }

  function applyManifest() {
    if (!preview || !window.confirm(`Apply this exact manifest preview to ${context} / ${scope || 'cluster scope'}? Earlier resources can remain applied if a later resource fails. No force ownership or automatic data rollback is used.`)) return
    void run(async () => {
      await apiSend('/api/kubernetes/admin/apply', 'POST', { context, namespace: scope, preview: preview.token, confirm: true })
      setPreview(null); listing.reload(); detail.reload()
      notifyNotice('kubernetes', 'Manifest operation queued. Follow acceptance, partial failures and workload conditions.')
    })
  }

  function workloadAction() {
    if (!selected || !window.confirm(`${selectedAction} ${selected.kind} ${selected.target.name} in ${context} / ${scope || 'cluster scope'}? Native RBAC, UID/resourceVersion and finalizers remain authoritative. Rollback does not restore persistent data.`)) return
    void run(async () => {
      await apiSend('/api/kubernetes/admin/actions', 'POST', { ...selected.target, action: selectedAction, replicas: selectedAction === 'scale' ? replicas : undefined, revision: selectedAction === 'rollback' ? revision : undefined, confirm: true, acknowledgeStorage })
      listing.reload(); detail.reload(); history.reload()
      notifyNotice('kubernetes', 'Resource operation queued. Inspect progress and conditions before assuming completion.')
    })
  }

  return <>
    <section className="fleetRail" aria-label="Kubernetes administration status">
      <span className="fleetRailTitle">Resources and native RBAC</span>
      <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
      <span className="fleetDatum">{context || 'Select context'} / {scope || 'cluster scope'}</span>
      <span className="fleetMessage">{access.error ? 'Authorization unavailable' : readOnly ? 'Read-only or not authorized to mutate' : 'Native permissions available'}</span>
    </section>
    <div className="controlBar kubernetesResourceControlBar">
      <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} disabled={busy} />
      <label className="namespaceField">Namespace<input type="text" value={resource?.namespaced ? namespace : ''} disabled={!resource?.namespaced || busy} placeholder={resource?.namespaced ? 'Explicit namespace required' : 'Cluster scope'} onChange={(event) => { setNamespace(event.target.value); reset() }} /></label>
      <label className="namespaceField">Served API resource<select value={resource ? resourceKey(resource) : ''} disabled={busy || !kinds.length} onChange={(event) => { setChosenResource(event.target.value); reset() }}>
        {kinds.map((entry) => <option key={resourceKey(entry)} value={resourceKey(entry)}>{entry.kind} / {entry.group || 'core'}/{entry.version} / {entry.resource}</option>)}
      </select></label>
      <button type="button" disabled={!ready || busy || !permitted('create')} onClick={createManifest}>New manifest</button>
      <button type="button" onClick={() => { status.reload(); discovery.reload(); listing.reload(); access.reload(); schema.reload() }}>Refresh discovery</button>
    </div>
    <div className="workArea">
      {incoming.error && <p role="alert">{incoming.error}</p>}
      {discovery.error && <p role="alert">{discovery.error}</p>}
      {discovery.data?.partial && <p role="alert">{discovery.data.message}</p>}
      {!available && <RuntimeGate label="Kubernetes administration" enabled={status.data?.enabled ?? false} message={status.error || status.data?.message} />}
      {available && <>
        <form className="inspectorForm inline" onSubmit={(event) => { event.preventDefault(); setSelectedName(nameDraft); setOpen(true); setManifest(''); setRevealed(null); setPreview(null) }}>
          <label>Resource name (optional)<input type="text" value={nameDraft} onChange={(event) => setNameDraft(event.target.value)} /></label>
          <button type="submit" disabled={!ready || !nameDraft || busy}>Inspect named resource</button>
          <button type="button" onClick={() => setShowSchema((value) => !value)}>{showSchema ? 'Hide served schema' : 'Show served schema'}</button>
        </form>
        {access.error && <p role="alert">{access.error}</p>}
        {access.data && <p>Effective permissions: {Object.entries(access.data.verbs).map(([verb, permission]) => `${verb}: ${permission.allowed ? 'allowed' : 'denied'}`).join(' / ')}. Viewing never implies mutation permission.</p>}
        {showSchema && <section className="drawerPanel"><h3>Served API schema</h3>{schema.error && <p role="alert">{schema.error}</p>}<p>{schema.data?.state} {schema.data?.message}</p><p className="mono">{schema.data?.version}</p>{schema.data?.definition && <pre className="definitionText">{JSON.stringify(schema.data.definition, null, 2)}</pre>}</section>}
        {listing.error && <p role="alert">{listing.error}</p>}
        <InventoryList items={listing.data?.items ?? []} getKey={(object) => object.target.name} selectedKey={selectedName} onSelect={select} columnsTemplate="minmax(160px,1fr) minmax(110px,.6fr) minmax(150px,1fr)" ariaLabel="Discovered Kubernetes resources" emptyMessage={listing.error || 'No resources in this explicit scope.'} columns={[
          { header: 'Name', render: (object) => <strong>{object.target.name}</strong> },
          { header: 'Kind', render: (object) => object.kind },
          { header: 'Identity', className: 'mono', render: (object) => `${object.target.uid ?? 'unknown'} / ${object.target.resourceVersion ?? 'unknown'}` },
        ]} />
        <div className="actions">{continuation && <button type="button" onClick={() => setContinuation('')}>First page</button>}{listing.data?.continue && <button type="button" onClick={() => setContinuation(listing.data?.continue ?? '')}>Next resource page</button>}</div>
      </>}
      {open && <Inspector title={selected?.target.name || `New ${resource?.kind ?? 'resource'}`} subtitle={`${context} / ${scope || 'cluster scope'} / ${resource?.version ?? ''}`} onClose={closeInspector}>
        {detail.error && <p role="alert">{detail.error}</p>}
        {selected && <section className="drawerPanel">
          <h3>{selected.secret ? 'Secret metadata (values hidden by default)' : 'Current resource and conditions'}</h3>
          <pre className="definitionText">{JSON.stringify(selected.manifest, null, 2)}</pre>
          {!selected.secret && <button type="button" disabled={busy || !permitted('patch')} onClick={() => { setManifest(JSON.stringify(selected.manifest, null, 2)); setMode('apply'); setPreview(null) }}>Edit manifest</button>}
          {selected.secret && <>
            <button type="button" disabled={busy || !permitted('get')} onClick={() => {
              if (!window.confirm(`Reveal Secret values for ${selected.target.name} in ${context} / ${scope}? Values stay local and are not added to Activity or cached previews.`)) return
              void run(async () => { setRevealed(await apiGet<AdminObject>(`/api/kubernetes/admin/resource?${adminQuery(selected.target)}&reveal=true`)) })
            }}>Reveal Secret values</button>
            {revealed && <><pre className="definitionText">{JSON.stringify(revealed.manifest, null, 2)}</pre><div className="actions">
              <button type="button" disabled={busy || !permitted('patch')} onClick={() => { setManifest(JSON.stringify(revealed.manifest, null, 2)); setMode('apply'); setPreview(null) }}>Edit revealed Secret</button>
              <button type="button" onClick={() => { setRevealed(null); setManifest(''); setSecretConsent(false); setPreview(null) }}>Hide Secret values</button>
            </div></>}
          </>}
          <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); workloadAction() }}>
            <label>Native resource action<select value={selectedAction} onChange={(event) => setAction(event.target.value)}>{actions.map((entry) => <option key={entry} value={entry}>{entry === 'scale' && resource?.resource === 'jobs' ? 'Set Job parallelism' : entry}</option>)}</select></label>
            {selectedAction === 'scale' && <label>Replicas / Job parallelism<input type="number" min="0" max="2147483647" required value={replicas} onChange={(event) => setReplicas(Number(event.target.value))} /></label>}
            {selectedAction === 'rollback' && <><label>Retained controller revision<select value={revision} onChange={(event) => setRevision(Number(event.target.value))}>{(history.data ?? []).map((entry) => <option key={entry.revision} value={entry.revision}>{entry.revision}: {entry.images.join(', ')}</option>)}</select></label>{history.error && <p role="alert">{history.error}</p>}</>}
            {storageAction && <label><input type="checkbox" checked={acknowledgeStorage} onChange={(event) => setAcknowledgeStorage(event.target.checked)} />I acknowledge native storage/namespace/CRD deletion and cascading data consequences</label>}
            <button type="submit" disabled={busy || !permitted('get') || !permitted(actionPermission) || storageAction && !acknowledgeStorage || selectedAction === 'rollback' && !history.data?.length}>Run resource action</button>
          </form>
        </section>}
        <section className="drawerPanel">
          <h3>Manifest create / edit</h3>
          <p>Server-owned status/metadata are preserved. UID/resourceVersion and native field-manager conflicts are checked again at apply. There is no force-overwrite control.</p>
          <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); previewManifest() }} onChange={() => setPreview(null)}>
            <label>Write mode<select value={mode} onChange={(event) => setMode(event.target.value)}><option value="apply">Conflict-aware server-side apply</option><option value="create">Create a new named resource</option></select></label>
            <label>Manifest YAML or JSON<textarea rows={16} aria-label="Kubernetes manifest editor" value={manifest} onChange={(event) => setManifest(event.target.value)} spellCheck={false} /></label>
            <button type="button" onClick={() => fileInput.current?.click()}>Upload manifest file</button>
            <input ref={fileInput} className="visuallyHidden" type="file" accept=".yaml,.yml,.json" aria-label="Kubernetes manifest file" onChange={(event) => {
              const file = event.target.files?.[0]; event.target.value = ''
              if (!file) return
              void run(async () => { if (file.size > 2 * 1024 * 1024) throw new Error('Manifest file exceeds 2 MiB.'); setManifest(await file.text()); setPreview(null) })
            }} />
            <label><input type="checkbox" checked={secretConsent} onChange={(event) => setSecretConsent(event.target.checked)} />Authorize Secret create/edit; values remain hidden in diffs and history</label>
            <button type="submit" disabled={busy || !manifest.trim() || !(mode === 'create' ? permitted('create') : permitted('patch'))}>Server dry run and diff</button>
          </form>
        </section>
        {preview && <section className="drawerPanel" aria-label="Kubernetes manifest preview">
          <h3>Server dry-run diff</h3>{preview.warnings.map((warning, index) => <p role="status" key={index}>{warning}</p>)}
          {preview.differences.map((difference) => <details key={`${difference.target.group}/${difference.target.resource}/${difference.target.name}`} open><summary>{difference.kind} {difference.target.namespace || 'cluster scope'}/{difference.target.name}</summary><p>Field managers: {difference.managers.join(', ') || 'none reported'}</p><h4>Before</h4><pre className="definitionText">{JSON.stringify(difference.before, null, 2)}</pre><h4>After</h4><pre className="definitionText">{JSON.stringify(difference.after, null, 2)}</pre></details>)}
          <button type="button" disabled={busy} onClick={applyManifest}>Apply manifest preview</button>
        </section>}
        <LocalOperations resource={`kubernetes:${context}`} />
      </Inspector>}
    </div>
  </>
}
