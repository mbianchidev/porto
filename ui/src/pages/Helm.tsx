import { useRef, useState } from 'react'
import { apiGet, apiSend, apiUpload, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useKubernetesResourceNames } from '../kubernetesResourceNames'
import { useMessages } from '../useMessages'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { LocalOperations } from '../components/LocalOperations'
import { StatusLamp } from '../components/StatusLamp'
import { ResourceInput } from '../components/ResourceInput'
import { adminQuery } from '../kubeAdminTypes'
import type { AdminAccess, HelmPreview, HelmRelease, HelmRepository } from '../kubeAdminTypes'
import type { KubernetesContext } from '../types'

export function Helm({ context, contexts, onContextChange }: { context: string; contexts: KubernetesContext[]; onContextChange: (context: string) => void }) {
  const { notifyError, notifyNotice } = useMessages()
  const [namespace, setNamespace] = useState('default')
  const [release, setRelease] = useState('')
  const [inspected, setInspected] = useState('')
  const [open, setOpen] = useState(false)
  const [action, setAction] = useState('upgrade')
  const [repository, setRepository] = useState('')
  const [chart, setChart] = useState('')
  const [version, setVersion] = useState('')
  const [chartID, setChartID] = useState('')
  const [values, setValues] = useState('{}')
  const [revealedValues, setRevealedValues] = useState<unknown>(null)
  const [revision, setRevision] = useState(1)
  const [secretConsent, setSecretConsent] = useState(false)
  const [acknowledgeStorage, setAcknowledgeStorage] = useState(false)
  const [preview, setPreview] = useState<HelmPreview | null>(null)
  const [busy, setBusy] = useState(false)
  const [repoName, setRepoName] = useState('')
  const [repoURL, setRepoURL] = useState('')
  const [repoUsername, setRepoUsername] = useState('')
  const [repoPassword, setRepoPassword] = useState('')
  const [publicRepo, setPublicRepo] = useState(false)
  const chartFile = useRef<HTMLInputElement>(null)
  const inspectorTrigger = useRef<HTMLElement | null>(null)
  const status = useKubernetesStatus(context)
  const ready = Boolean(context && namespace.trim() && status.data?.available)
  const scopeQuery = new URLSearchParams({ context, namespace }).toString()
  const repositories = usePolledResource<HelmRepository[]>((signal) => apiGet('/api/kubernetes/helm/repositories', signal), 0, [])
  const releases = usePolledResource<HelmRelease[]>((signal) => ready ? apiGet(`/api/kubernetes/helm/releases?${scopeQuery}`, signal) : Promise.resolve([]), 6000, [ready, scopeQuery])
  const namespaces = useKubernetesResourceNames({ context, namespace: '', group: '', version: 'v1', resource: 'namespaces' }, Boolean(context && status.data?.available))
  const detail = usePolledResource<HelmRelease | null>((signal) => ready && inspected ? apiGet(`/api/kubernetes/helm/release?${scopeQuery}&release=${encodeURIComponent(inspected)}`, signal) : Promise.resolve(null), 3000, [ready, scopeQuery, inspected])
  const history = usePolledResource<HelmRelease[]>((signal) => ready && inspected ? apiGet(`/api/kubernetes/helm/history?${scopeQuery}&release=${encodeURIComponent(inspected)}`, signal) : Promise.resolve([]), 0, [ready, scopeQuery, inspected])
  const access = usePolledResource<AdminAccess | null>((signal) => ready ? apiGet(`/api/kubernetes/admin/access?${adminQuery({ context, namespace, group: '', version: 'v1', resource: 'secrets', name: '' })}`, signal) : Promise.resolve(null), 10000, [ready, context, namespace])
  const permitted = (verb: string) => Boolean(access.data?.verbs[verb]?.allowed)
  const canWrite = permitted('get') && permitted('list') && permitted('update') && (action === 'uninstall' || permitted('create'))

  async function run(task: () => Promise<void>) {
    setBusy(true)
    try { await task() } catch (error) { notifyError('helm', errorMessage(error, 'Helm operation failed')) } finally { setBusy(false) }
  }

  function openRelease(entry?: HelmRelease) {
    inspectorTrigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setRelease(entry?.name ?? ''); setInspected(entry?.name ?? ''); setOpen(true); setAction('upgrade'); setPreview(null); setValues('{}'); setRevealedValues(null); setSecretConsent(false); setAcknowledgeStorage(false)
  }

  function closeInspector() {
    setOpen(false); setValues('{}'); setRevealedValues(null); setPreview(null); setSecretConsent(false)
    requestAnimationFrame(() => { if (inspectorTrigger.current?.isConnected) inspectorTrigger.current.focus() })
  }

  function prepare() {
    void run(async () => {
      const result = await apiSend<HelmPreview>('/api/kubernetes/helm/previews', 'POST', { context, namespace, release, action, repository, chart, version, chartId: chartID, values, revision, secretConsent })
      setPreview(result); setValues('{}'); setRevealedValues(null); setSecretConsent(false); setAcknowledgeStorage(false)
    })
  }

  function apply() {
    if (!preview || !window.confirm(`Run Helm ${preview.action} for ${preview.release} in ${context} / ${namespace}? Hooks can leave partial resources. No automatic data rollback, force ownership or cleanup is enabled.`)) return
    void run(async () => {
      await apiSend('/api/kubernetes/helm/apply', 'POST', { context, namespace, preview: preview.token, confirm: true, acknowledgeStorage })
      setPreview(null); setInspected(release); releases.reload(); detail.reload(); history.reload()
      notifyNotice('helm', 'Helm operation queued. Follow native release state, hooks and operation history.')
    })
  }

  return <>
    <section className="fleetRail" aria-label="Helm status">
      <span className="fleetRailTitle">Native Helm lifecycle</span>
      <span className="fleetDatum"><StatusLamp state={ready ? 'running' : 'crashed'} />{ready ? 'Selected context available' : 'Select an available context'}</span>
      <span className="fleetDatum">{context || 'No context'} / {namespace || 'Namespace required'}</span>
      <span className="fleetMessage">Helm 4.3.1 / native Secret-store RBAC / no global context fallback</span>
    </section>
    <div className="controlBar">
      <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} disabled={busy} />
      <label className="namespaceField">Namespace<ResourceInput options={namespaces.error ? [] : namespaces.data ?? []} type="text" required value={namespace} onChange={(event) => { setNamespace(event.target.value); setPreview(null); setRevealedValues(null); setValues('{}'); setInspected(''); setOpen(false) }} /></label>
      <button type="button" disabled={!ready || busy || !permitted('create')} onClick={() => openRelease()}>Install or upgrade release</button>
      <button type="button" onClick={() => { status.reload(); releases.reload(); namespaces.reload(); access.reload(); history.reload(); detail.reload() }}>Refresh Helm state</button>
    </div>
    <div className="workArea">
      {status.error && <p role="alert">{status.error}</p>}
      {access.error && <p role="alert">{access.error}</p>}
      {access.data && <p>Release-store permissions: {Object.entries(access.data.verbs).map(([verb, permission]) => `${verb}: ${permission.allowed ? 'allowed' : 'denied'}`).join(' / ')}. Chart resources have their own native RBAC/admission checks.</p>}
      {releases.error && <p role="alert">{releases.error}</p>}
      {namespaces.error && <p role="alert">Namespace suggestions unavailable: {namespaces.error} You can still enter an explicit namespace.</p>}
      <InventoryList items={releases.data ?? []} getKey={(entry) => entry.name} selectedKey={inspected} onSelect={openRelease} columnsTemplate="minmax(160px,1fr) minmax(100px,.6fr) minmax(160px,1fr) minmax(70px,.4fr)" ariaLabel="Helm releases in selected namespace" emptyMessage={releases.error || 'No Helm releases in this explicit namespace.'} columns={[
        { header: 'Release', render: (entry) => <strong>{entry.name}</strong> }, { header: 'Status', render: (entry) => entry.status }, { header: 'Chart', render: (entry) => entry.chart }, { header: 'Revision', render: (entry) => entry.revision },
      ]} />
      <section className="drawerPanel">
        <h3>Local Helm repositories</h3>
        <p>Sources use scoped HTTPS credentials. Updating indexes and pulling a chart are explicit network operations; private tokens are not URL or command arguments.</p>
        {repositories.error && <p role="alert">{repositories.error}</p>}
        <form className="inspectorForm" onSubmit={(event) => {
          event.preventDefault()
          void run(async () => {
            await apiSend('/api/kubernetes/helm/repositories', 'POST', { name: repoName, url: repoURL, username: repoUsername, password: publicRepo ? '' : repoPassword || undefined })
            setRepoPassword(''); repositories.reload(); notifyNotice('helm', 'Scoped Helm repository saved. Update indexes explicitly before selecting a chart.')
          })
        }}>
          <label>Repository name<input required value={repoName} onChange={(event) => setRepoName(event.target.value)} pattern="[a-z0-9][a-z0-9-]{0,52}" /></label>
          <label>Repository HTTPS URL<input type="url" required value={repoURL} onChange={(event) => setRepoURL(event.target.value)} placeholder="https://charts.example.com/" /></label>
          <label>Repository username<input value={repoUsername} onChange={(event) => setRepoUsername(event.target.value)} /></label>
          <label>Repository password/token<input type="password" autoComplete="new-password" value={repoPassword} onChange={(event) => setRepoPassword(event.target.value)} placeholder="Leave blank to retain existing scoped credentials" /></label>
          <label><input type="checkbox" checked={publicRepo} onChange={(event) => setPublicRepo(event.target.checked)} />Use public source access and clear the stored credential</label>
          <div className="actions"><button type="submit" disabled={busy}>Save repository</button><button type="button" disabled={busy || !repositories.data?.length} onClick={() => void run(async () => { await apiSend('/api/kubernetes/helm/repositories/update', 'POST'); notifyNotice('helm', 'Repository index update queued.') })}>Update repository indexes</button></div>
        </form>
        {(repositories.data ?? []).map((entry) => <div className="actions" key={entry.name}><span>{entry.name} / {entry.url} / {entry.credentialStored ? 'stored credentials' : 'public access'}</span><button type="button" disabled={busy} onClick={() => {
          if (!window.confirm(`Remove local repository ${entry.name}? Installed releases and storage are not removed.`)) return
          void run(async () => { await apiSend(`/api/kubernetes/helm/repositories/${encodeURIComponent(entry.name)}`, 'DELETE'); repositories.reload() })
        }}>Remove repository</button></div>)}
      </section>
      {open && <Inspector title={release || 'Helm lifecycle'} subtitle={`${context} / ${namespace}`} onClose={closeInspector}>
        <section className="drawerPanel">
          <h3>Release state and hooks</h3>
          {detail.error && <p role="alert">{detail.error}</p>}
          {detail.data && <><p>{detail.data.status} / revision {detail.data.revision} / {detail.data.chart}</p>{detail.data.hooks?.map((hook) => <p key={`${hook.kind}/${hook.name}`}>{hook.kind} {hook.name}: {hook.phase || 'not observed'}</p>)}</>}
          <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); prepare() }} onChange={() => setPreview(null)}>
            <label>Release name<ResourceInput options={(releases.data ?? []).map((entry) => entry.name)} required pattern="[a-z0-9][a-z0-9-]{0,52}" value={release} onChange={(event) => setRelease(event.target.value)} /></label>
            <label>Helm action<select value={action} onChange={(event) => setAction(event.target.value)}><option value="upgrade">Install / upgrade selected chart</option><option value="rollback">Rollback retained revision</option><option value="uninstall">Uninstall, retaining release history</option></select></label>
            {action === 'upgrade' && <>
              <label>Chart repository<select value={repository} onChange={(event) => setRepository(event.target.value)}><option value="">Select a configured source</option>{(repositories.data ?? []).map((entry) => <option key={entry.name} value={entry.name}>{entry.name}</option>)}</select></label>
              <label>Chart name<input required={!chartID} value={chart} onChange={(event) => setChart(event.target.value)} /></label>
              <label>Exact chart version<input required={!chartID} value={version} onChange={(event) => setVersion(event.target.value)} placeholder="1.0.0" /></label>
              <button type="button" onClick={() => chartFile.current?.click()}>Upload local chart archive</button>
              <input ref={chartFile} className="visuallyHidden" type="file" accept=".tgz,.tar.gz" aria-label="Helm chart archive" onChange={(event) => {
                const file = event.target.files?.[0]; event.target.value = ''
                if (!file) return
                void run(async () => {
                  if (file.size > 16 * 1024 * 1024) throw new Error('Chart archives must be at most 16 MiB.')
                  const result = await apiUpload<{ chartId: string }>('/api/kubernetes/helm/charts', file)
                  setChartID(result.chartId); setPreview(null)
                })
              }} />
              {chartID && <><p className="mono">Pinned uploaded chart: {chartID}</p><button type="button" onClick={() => { setChartID(''); setPreview(null) }}>Use repository chart instead</button></>}
              <label>Replacement Helm values (JSON, sensitive)<input type="password" autoComplete="new-password" value={values} onChange={(event) => setValues(event.target.value)} /></label>
              <p>Values replace previous overrides. Explicitly load existing values if retaining them. Secret-bearing values are cleared from this input after previewing.</p>
            </>}
            {action === 'rollback' && <label>Retained Helm revision<input type="number" min="1" required value={revision} onChange={(event) => setRevision(Number(event.target.value))} /></label>}
            <label><input type="checkbox" checked={secretConsent} onChange={(event) => setSecretConsent(event.target.checked)} />Authorize chart/release rendering and Secret changes; public diffs hide Kubernetes Secret values</label>
            <div className="actions">
              <button type="submit" disabled={busy || !ready || !canWrite || !secretConsent}>Render, dry run and preview Helm</button>
              <button type="button" disabled={busy || !release} onClick={() => { setInspected(release); detail.reload(); history.reload() }}>Inspect release and history</button>
            </div>
          </form>
          {inspected && <button type="button" disabled={busy || !permitted('get')} onClick={() => {
            if (!window.confirm(`Reveal all values for ${inspected} in ${context} / ${namespace}? They may contain Secrets and will not be added to Activity.`)) return
            void run(async () => { setRevealedValues(await apiGet<unknown>(`/api/kubernetes/helm/values?${scopeQuery}&release=${encodeURIComponent(inspected)}&reveal=true`)) })
          }}>Reveal current Helm values</button>}
          {revealedValues !== null && <><pre className="definitionText">{JSON.stringify(revealedValues, null, 2)}</pre><div className="actions"><button type="button" onClick={() => { setValues(JSON.stringify(revealedValues)); setPreview(null); setRevealedValues(null) }}>Use these values as overrides</button><button type="button" onClick={() => { setRevealedValues(null); setValues('{}') }}>Hide Helm values</button></div></>}
        </section>
        {history.error && <p role="alert">{history.error}</p>}
        {(history.data ?? []).length > 0 && <section className="drawerPanel"><h3>Native release history</h3>{history.data?.map((entry) => <p key={entry.revision}>Revision {entry.revision}: {entry.status} / {entry.chart} / {entry.updated}</p>)}</section>}
        {preview && <section className="drawerPanel" aria-label="Helm lifecycle preview">
          <h3>Rendered Helm lifecycle diff</h3>
          <p>Release revision {preview.revision}; pinned chart {preview.chartId || 'native retained release history'}.</p>
          {preview.warnings.map((warning, index) => <p role="status" key={index}>{warning}</p>)}
          <h4>Before</h4><pre className="definitionText">{JSON.stringify(preview.before, null, 2)}</pre><h4>After</h4><pre className="definitionText">{JSON.stringify(preview.after, null, 2)}</pre>
          {preview.storageConsequences.map((consequence, index) => <p role="alert" key={index}>{consequence}</p>)}
          {preview.storageConsequences.length > 0 && <label className="inspectorForm"><span><input type="checkbox" checked={acknowledgeStorage} onChange={(event) => setAcknowledgeStorage(event.target.checked)} />I acknowledge rendered storage, namespace, CRD and dependent-data lifecycle consequences</span></label>}
          <button type="button" disabled={busy || preview.storageConsequences.length > 0 && !acknowledgeStorage} onClick={apply}>Apply Helm preview</button>
        </section>}
        <LocalOperations resource={`kubernetes:${context}`} />
      </Inspector>}
      <LocalOperations resource="helm:repositories" />
    </div>
  </>
}
