import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useKubernetesResourceNames } from '../kubernetesResourceNames'
import { useMessages } from '../useMessages'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { ResourceInput } from '../components/ResourceInput'
import { StatusLamp } from '../components/StatusLamp'
import { LocalOperations } from '../components/LocalOperations'
import { gitOpsQuery } from '../gitOpsTypes'
import type { GitOpsApplication, GitOpsController, GitOpsPreview, GitOpsScope, GitOpsSource } from '../gitOpsTypes'
import type { KubernetesContext } from '../types'

type Props = { context: string; contexts: KubernetesContext[]; onContextChange: (context: string) => void; operateAllowed: boolean; infrastructureAllowed: boolean }
const initialForm = { name: '', kind: 'Kustomization', source: '', revision: '', path: './deploy', chart: '', renderer: 'kustomize', serviceAccount: '', project: '', dependencies: '', inventory: '[]', helmRelease: '' }

export function GitOps({ context, contexts, onContextChange, operateAllowed, infrastructureAllowed }: Props) {
  const [namespace, setNamespace] = useState('default')
  const [controller, setController] = useState<GitOpsScope['controller']>('flux')
  const [controllerNamespace, setControllerNamespace] = useState('flux-system')
  const status = useKubernetesStatus(context)
  const namespaces = useKubernetesResourceNames({ context, namespace: '', group: '', version: 'v1', resource: 'namespaces' }, Boolean(context && status.data?.available))
  const scope: GitOpsScope = { context, namespace, controller, controllerNamespace }
  const valid = Boolean(context && namespace && controllerNamespace && status.data?.available && !status.error)
  return <>
    <section className="fleetRail" aria-label="Kubernetes GitOps delivery">
      <span className="fleetRailTitle">One desired-state owner</span>
      <span className="fleetDatum"><StatusLamp state={valid ? 'running' : 'stopped'} />{context || 'Select a context'} / {namespace || 'Select a namespace'}</span>
      <span className="fleetMessage">Native reconciliation, not Compose polling or direct Helm administration</span>
    </section>
    <div className="controlBar">
      <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
      <label className="namespaceField">Application namespace<ResourceInput options={namespaces.data ?? []} required value={namespace} onChange={(event) => setNamespace(event.target.value)} /></label>
      <label>GitOps controller<select value={controller} onChange={(event) => {
        const next = event.target.value === 'argocd' ? 'argocd' : 'flux'
        setController(next); setControllerNamespace(next === 'flux' ? 'flux-system' : 'argocd')
      }}><option value="flux">Flux</option><option value="argocd">Argo CD</option></select></label>
      <label>Controller namespace<input required value={controllerNamespace} onChange={(event) => setControllerNamespace(event.target.value)} /></label>
    </div>
    <div className="workArea">
      {status.error && <p role="alert">{status.error}</p>}
      {namespaces.error && <p role="alert">Namespace suggestions unavailable: {namespaces.error} Enter an explicit authorized namespace.</p>}
      {!valid ? <p role="status">Select an available context and explicit application/controller namespaces. No controller is installed automatically.</p>
        : <GitOpsWorkspace key={gitOpsQuery(scope)} scope={scope} operateAllowed={operateAllowed} infrastructureAllowed={infrastructureAllowed} />}
    </div>
  </>
}

function GitOpsWorkspace({ scope, operateAllowed, infrastructureAllowed }: { scope: GitOpsScope; operateAllowed: boolean; infrastructureAllowed: boolean }) {
  const { notifyError, notifyNotice } = useMessages()
  const query = gitOpsQuery(scope)
  const controller = usePolledResource<GitOpsController>((signal) => apiGet(`/api/kubernetes/gitops/controller?${query}`, signal), 6000, [query])
  const sources = usePolledResource<GitOpsSource[]>((signal) => apiGet(`/api/kubernetes/gitops/sources?${query}`, signal), 0, [query])
  const installed = Boolean(controller.data?.installed)
  const applications = usePolledResource<GitOpsApplication[]>((signal) => installed ? apiGet(`/api/kubernetes/gitops/applications?${query}`, signal) : Promise.resolve([]), 6000, [query, installed])
  const [selected, setSelected] = useState<GitOpsApplication | null>(null)
  const [open, setOpen] = useState(false)
  const [form, setForm] = useState(initialForm)
  const [action, setAction] = useState('install')
  const [automatic, setAutomatic] = useState(false)
  const [selfHeal, setSelfHeal] = useState(false)
  const [prune, setPrune] = useState(false)
  const [deletePolicy, setDeletePolicy] = useState('orphan')
  const [preview, setPreview] = useState<GitOpsPreview | null>(null)
  const [controllerPreview, setControllerPreview] = useState<GitOpsPreview | null>(null)
  const [controllerAction, setControllerAction] = useState('bootstrap')
  const [busy, setBusy] = useState(false)
  const [sourceName, setSourceName] = useState('')
  const [sourceKind, setSourceKind] = useState('git')
  const [sourceURL, setSourceURL] = useState('')
  const [sourceUsername, setSourceUsername] = useState('')
  const [sourcePassword, setSourcePassword] = useState('')
  const [publicSource, setPublicSource] = useState(true)
  const trigger = useRef<HTMLElement | null>(null)
  const detail = usePolledResource<GitOpsApplication | null>((signal) => selected ? apiGet(`/api/kubernetes/gitops/application?${query}&kind=${encodeURIComponent(selected.kind)}&name=${encodeURIComponent(selected.name)}`, signal) : Promise.resolve(null), 3000, [query, selected?.kind, selected?.name], `gitops:${query}:${selected?.kind}:${selected?.name}`)
  const current = detail.data?.name === selected?.name && detail.data?.kind === selected?.kind ? detail.data : null
  const canWrite = Boolean(operateAllowed && controller.data?.canWrite && controller.data.supported && controller.data.ready && controller.data.protected && !controller.error)
  const canSaveSource = Boolean(operateAllowed && controller.data?.canWrite && !controller.error)
  const canInstallController = Boolean(infrastructureAllowed && controller.data?.canBootstrap && !controller.error)
  const configuring = ['install', 'update', 'adopt'].includes(action)
  const source = sources.data?.find((entry) => entry.name === form.source)

  async function run(task: () => Promise<void>) {
    setBusy(true)
    try { await task() } catch (error) { notifyError('gitops', errorMessage(error, 'GitOps operation failed')) } finally { setBusy(false) }
  }

  function refresh() { controller.reload(); sources.reload(); applications.reload(); detail.reload() }
  function field(name: keyof typeof initialForm, value: string) { setForm((previous) => ({ ...previous, [name]: value })); setPreview(null) }
  function inspect(entry?: GitOpsApplication) {
    trigger.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelected(entry ?? null); setOpen(true); setPreview(null); setAction(entry ? 'reconcile' : 'install')
    setForm({ ...initialForm, kind: entry?.kind ?? (scope.controller === 'flux' ? 'Kustomization' : 'Application'), name: entry?.name ?? '', source: entry?.source ?? '', revision: entry?.desiredRevision ?? '',
      path: entry?.configuration.path ?? './deploy', chart: entry?.configuration.chart ?? '', renderer: entry?.configuration.renderer ?? 'kustomize',
      project: entry?.configuration.project ?? '', serviceAccount: entry?.configuration.serviceAccount ?? '', dependencies: entry?.configuration.dependencies.join(', ') ?? '' })
    setAutomatic(entry?.automatic ?? false); setSelfHeal(entry?.selfHeal ?? false); setPrune(entry?.prune ?? false); setDeletePolicy('orphan')
  }
  function close() {
    setOpen(false); setSelected(null); setPreview(null)
    requestAnimationFrame(() => { if (trigger.current?.isConnected) trigger.current.focus() })
  }
  function prepare() {
    void run(async () => {
      let inventory: unknown = []
      if (action === 'adopt' && form.kind !== 'HelmRelease') {
        inventory = JSON.parse(form.inventory)
        if (!Array.isArray(inventory)) throw new Error('Ownership handoff requires a JSON array of exact scoped resource identities.')
      }
      const request = { ...scope, action, name: form.name, kind: form.kind, uid: current?.target.uid, resourceVersion: current?.target.resourceVersion,
        source: form.source, revision: form.revision, path: form.path, chart: form.chart, renderer: form.renderer,
        serviceAccount: form.serviceAccount, project: form.project, dependencies: form.dependencies.split(',').map((name) => name.trim()).filter(Boolean),
        automatic: action === 'resume' && automatic, selfHeal: action === 'resume' && selfHeal, prune: ['resume', 'reconcile', 'recover'].includes(action) && prune,
        deletePolicy, inventory, helmRelease: form.helmRelease }
      setPreview(await apiSend<GitOpsPreview>('/api/kubernetes/gitops/previews', 'POST', request))
    })
  }
  function apply(value: GitOpsPreview, acknowledgeData: boolean, controllerLifecycle: boolean) {
    if (!window.confirm(`Apply ${value.action} for ${value.name} in ${scope.context} / ${scope.namespace} using ${scope.controller} in ${scope.controllerNamespace}? Native finalizers, pruning and storage policies apply. No automatic data rollback.`)) return
    void run(async () => {
      await apiSend(`/api/kubernetes/gitops/${controllerLifecycle ? 'controller/' : ''}apply`, 'POST', { ...scope, preview: value.token, confirm: true, acknowledgeData })
      if (controllerLifecycle) setControllerPreview(null); else setPreview(null)
      refresh(); notifyNotice('gitops', 'GitOps operation queued. Follow native source/reconciliation/health state and operation history; queued does not mean ready.')
    })
  }
  return <>
    <h1>GitOps applications</h1>
    {controller.error && <p role="alert">{controller.error}</p>}
    {controller.data && <p role="status">{scope.controller === 'flux' ? 'Flux' : 'Argo CD'} / bootstrap pin {controller.data.bootstrapVersion} / {controller.data.message}</p>}
    {!canWrite && <p>Read-only or not authorized to mutate this controller</p>}
    <div className="actions">
      <button type="button" disabled={!canWrite || busy} onClick={() => inspect()}>Register GitOps application</button>
      <button type="button" onClick={refresh}>Refresh GitOps state</button>
    </div>
    {applications.error && <p role="alert">{applications.error}</p>}
    <InventoryList items={applications.data ?? []} getKey={(entry) => `${entry.kind}/${entry.name}`} selectedKey={selected ? `${selected.kind}/${selected.name}` : ''} onSelect={inspect}
      columnsTemplate="minmax(150px,1fr) minmax(100px,.6fr) minmax(100px,.6fr) minmax(130px,.8fr)" ariaLabel="GitOps applications in selected namespace"
      emptyMessage={applications.error || (installed ? 'No native GitOps applications in this selected namespace.' : 'Discover or explicitly bootstrap a controller before application delivery.')} columns={[
        { header: 'Application / owner', render: (entry) => <><strong>{entry.name}</strong><br />{entry.kind}{!entry.managed && ' / external owner'}</> },
        { header: 'Reconciliation', render: (entry) => <>{entry.phase}{entry.failureStage && <p>{entry.failureStage}</p>}</> },
        { header: 'Workload health', render: (entry) => <>{entry.health}<br />drift: {entry.drift}</> },
        { header: 'Desired / applied', render: (entry) => <span className="mono">{entry.desiredRevision.slice(0, 12) || 'Unknown'}<br />{entry.appliedRevision.slice(0, 12) || 'Not observed'}</span> },
      ]} />
    <details className="drawerPanel"><summary>Connect a source</summary>
      <h3>Local source registrations</h3>
      <p>HTTPS access is scoped to this context, controller and namespace. Private credentials are encrypted with the system credential store, then provisioned only into reviewed native source Secrets.</p>
      {sources.error && <p role="alert">{sources.error}</p>}
      <form className="inspectorForm" onSubmit={(event) => {
        event.preventDefault()
        void run(async () => {
          await apiSend('/api/kubernetes/gitops/sources', 'POST', { ...scope, name: sourceName, kind: sourceKind, url: sourceURL, username: sourceUsername, password: publicSource ? '' : sourcePassword || undefined })
          setSourcePassword(''); sources.reload(); notifyNotice('gitops', 'Scoped source saved. Native applications and reconciliation were not changed.')
        })
      }}>
        <label>Source name<input required value={sourceName} onChange={(event) => setSourceName(event.target.value)} /></label>
        <label>Source kind<select value={sourceKind} onChange={(event) => setSourceKind(event.target.value)}><option value="git">Git</option><option value="helm">Helm repository</option></select></label>
        <label>Source HTTPS URL<input type="url" required value={sourceURL} onChange={(event) => setSourceURL(event.target.value)} /></label>
        <label><input type="checkbox" checked={publicSource} onChange={(event) => { setPublicSource(event.target.checked); setSourcePassword('') }} />Use public source access</label>
        {!publicSource && <><label>Source username<input value={sourceUsername} onChange={(event) => setSourceUsername(event.target.value)} autoComplete="off" /></label><label>Source password/token<input type="password" autoComplete="new-password" value={sourcePassword} onChange={(event) => setSourcePassword(event.target.value)} placeholder="Blank retains this exact source's existing credential" /></label></>}
        <button type="submit" disabled={!canSaveSource || busy}>Save scoped source</button>
      </form>
      {(sources.data ?? []).map((entry) => <div className="actions" key={entry.name}><span>{entry.name} / {entry.kind} / {entry.url} / {entry.credentialStored ? 'stored credentials' : 'public access'}</span>
        <button type="button" disabled={!canSaveSource || busy} onClick={() => {
          if (!window.confirm(`Remove local source registration ${entry.name}? Native source Secrets, applications, controllers and storage remain installed.`)) return
          void run(async () => { await apiSend(`/api/kubernetes/gitops/sources/${encodeURIComponent(entry.name)}?${query}`, 'DELETE'); sources.reload() })
        }}>Remove local registration</button></div>)}
    </details>
    <details className="drawerPanel"><summary>Controller installation</summary>
      <p>Installation/upgrade/removal is separate from application lifecycle. Only the chosen controller is installed. Foreign installations are discovered, never force-adopted. Removal preserves namespaces/CRDs and is blocked while any application remains.</p>
      <label>Controller lifecycle<select value={controllerAction} onChange={(event) => { setControllerAction(event.target.value); setControllerPreview(null) }}>
        <option value="bootstrap">Bootstrap or recover pinned controller</option><option value="protect-ownership">Enable selected namespace ownership guard</option><option value="upgrade-controller">Upgrade Porto-owned controller to pin</option><option value="remove-controller">Remove Porto-owned controller components</option>
      </select></label>
      <button type="button" disabled={busy || !canInstallController || !['bootstrap', 'protect-ownership'].includes(controllerAction) && !controller.data?.managed} onClick={() => void run(async () => {
        setControllerPreview(await apiSend<GitOpsPreview>('/api/kubernetes/gitops/controller/previews', 'POST', { ...scope, action: controllerAction }))
      })}>Preview controller lifecycle</button>
      {controllerPreview && <GitOpsPreviewPanel key={controllerPreview.token} preview={controllerPreview} busy={busy} onApply={(acknowledge) => apply(controllerPreview, acknowledge, true)} />}
    </details>
    {open && <Inspector title={form.name || 'GitOps application'} subtitle={`${scope.context} / ${scope.namespace} / ${scope.controller}`} onClose={close}>
      {detail.error && <p role="alert">{detail.error}</p>}
      {current && <section className="drawerPanel">
        <h3>Reconciliation is not readiness</h3>
        <div className="gitOpsRevisionRail">
          <div><small>Desired</small><span className="mono">{current.desiredRevision || 'Unknown immutable revision'}</span></div>
          <div><small>Source artifact</small><span className="mono">{current.sourceRevision || 'Not observed'}</span></div>
          <div><small>Applied</small><span className="mono">{current.appliedRevision || 'Not observed'}</span></div>
          <div><small>Workload health</small><strong>{current.health}</strong><span>{current.phase} / drift {current.drift}</span></div>
        </div>
        <p>{current.message}</p><p>Dependencies: {current.dependenciesReady ? 'ready' : 'not ready'} / last successful reconciliation: {current.lastSuccess || 'not reported by controller'} / adoption: {current.adoption || 'not staged'}</p>
        <p>Automatic reconciliation: {current.automatic ? 'enabled' : 'disabled or suspended'} / self-heal while running: {current.selfHeal ? 'enabled' : 'disabled'} / pruning: {current.prune ? 'native policy enabled' : 'disabled'}</p>
        {current.failureStage && <p role="alert">Failure stage: {current.failureStage}</p>}
        {current.conditions.map((condition, index) => <p key={index}>{condition.type}: {condition.status} / {condition.reason} {condition.transitionAt}</p>)}
        {current.inventory.length > 0 && <details><summary>Native resource inventory</summary>{current.inventory.map((entry, index) => <p key={index}>{entry.kind} {entry.namespace || 'cluster'}/{entry.name}</p>)}</details>}
        {!current.managed && <p role="alert">This root has an external desired-state owner. Its source, parent controller and ownership must be handled through that owner; Porto does not create a second writer.</p>}
      </section>}
      <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); prepare() }} onChange={() => setPreview(null)}>
        <label>GitOps action<select value={action} onChange={(event) => setAction(event.target.value)}>
          {!selected ? <><option value="install">Register suspended application</option><option value="adopt">Stage ownership handoff</option></> : <>
            <option value="reconcile">Reconcile / sync intended revision</option><option value="suspend">Suspend reconciliation</option><option value="resume">Resume reviewed policy</option><option value="update">Update suspended configuration</option><option value="recover">Recover a compatible desired revision once</option><option value="verify-adoption">Verify staged ownership handoff</option><option value="delete">Delete application with explicit policy</option>
          </>}
        </select></label>
        <label>Application name<input required value={form.name} readOnly={Boolean(selected)} onChange={(event) => field('name', event.target.value)} /></label>
        {!selected && scope.controller === 'flux' && <label>Flux application kind<select value={form.kind} onChange={(event) => field('kind', event.target.value)}><option value="Kustomization">Kustomization</option><option value="HelmRelease">HelmRelease</option></select></label>}
        {configuring && <>
          <label>Connected source<select required value={form.source} onChange={(event) => field('source', event.target.value)}><option value="">Select a scoped source</option>{(sources.data ?? []).map((entry) => <option key={entry.name} value={entry.name}>{entry.name} ({entry.kind})</option>)}</select></label>
          <label>Immutable Git commit / exact chart version<input required value={form.revision} onChange={(event) => field('revision', event.target.value)} /></label>
          {source?.kind !== 'helm' && <label>Repository path<input required value={form.path} onChange={(event) => field('path', event.target.value)} /></label>}
          {source?.kind === 'helm' && <label>Chart name<input required value={form.chart} onChange={(event) => field('chart', event.target.value)} /></label>}
          {scope.controller === 'flux' ? <><label>Reconciliation service account<input required value={form.serviceAccount} onChange={(event) => field('serviceAccount', event.target.value)} /></label><label>Same-namespace dependencies (comma-separated)<input value={form.dependencies} onChange={(event) => field('dependencies', event.target.value)} /></label></>
            : <><label>Argo CD project<input required value={form.project} onChange={(event) => field('project', event.target.value)} /></label>{source?.kind !== 'helm' && <label>Git renderer<select value={form.renderer} onChange={(event) => field('renderer', event.target.value)}><option value="kustomize">Kustomize</option><option value="directory">Manifest directory</option></select></label>}</>}
          <p>Registration and configuration updates stay suspended. Review a separate sync/resume before controllers change workloads.</p>
        </>}
        {action === 'adopt' && (form.kind === 'HelmRelease' ? <label>Existing native Helm release (same application name)<input required value={form.helmRelease} onChange={(event) => field('helmRelease', event.target.value)} /></label>
          : <label>Exact resource identities for handoff (JSON)<textarea required rows={8} value={form.inventory} onChange={(event) => field('inventory', event.target.value)} spellCheck={false} /></label>)}
        {action === 'recover' && <label>Compatible immutable recovery revision<input required value={form.revision} onChange={(event) => field('revision', event.target.value)} /></label>}
        {action === 'resume' && <>
          {scope.controller === 'argocd' && <label><input type="checkbox" checked={automatic} onChange={(event) => setAutomatic(event.target.checked)} />Enable automatic synchronization</label>}
          <label><input type="checkbox" checked={selfHeal} onChange={(event) => setSelfHeal(event.target.checked)} />Enable self-heal / drift correction while running</label>
        </>}
        {['resume', 'reconcile', 'recover'].includes(action) && <label><input type="checkbox" checked={prune} onChange={(event) => setPrune(event.target.checked)} />Enable pruning for this policy (native Helm upgrades inherently prune removed chart resources)</label>}
        {action === 'delete' && <label>Application deletion policy<select value={deletePolicy} onChange={(event) => setDeletePolicy(event.target.value)}><option value="orphan">Orphan / preserve workloads and data</option><option value="prune">Controller-native prune / uninstall</option></select></label>}
        <button type="submit" disabled={!canWrite || busy || Boolean(selected && (!current?.managed || detail.error))}>Preview GitOps operation</button>
      </form>
      {preview && <GitOpsPreviewPanel key={preview.token} preview={preview} busy={busy} onApply={(acknowledge) => apply(preview, acknowledge, false)} />}
      <LocalOperations resource={`kubernetes:${scope.context}`} />
    </Inspector>}
    {!open && <LocalOperations resource={`kubernetes:${scope.context}`} />}
  </>
}

function GitOpsPreviewPanel({ preview, busy, onApply }: { preview: GitOpsPreview; busy: boolean; onApply: (acknowledgeData: boolean) => void }) {
  const [acknowledge, setAcknowledge] = useState(false)
  return <section className="drawerPanel" aria-label="GitOps operation preview">
    <h3>{preview.action}: {preview.name}</h3>
    <p>{preview.context} / {preview.namespace} / {preview.controller} in {preview.controllerNamespace}. Expires: {preview.expiresAt}</p>
    {preview.warnings.map((warning, index) => <p role="status" key={index}>{warning}</p>)}
    {preview.differences.map((difference, index) => <details key={index} open={preview.differences.length < 4}><summary>{difference.kind} {difference.target.namespace || 'cluster'}/{difference.target.name}</summary><h4>Before</h4><pre className="definitionText">{JSON.stringify(difference.before, null, 2)}</pre><h4>After</h4><pre className="definitionText">{JSON.stringify(difference.after, null, 2)}</pre></details>)}
    {preview.consequences.map((consequence, index) => <p role="alert" key={index}>{consequence}</p>)}
    {preview.consequences.length > 0 && <label className="inspectorForm"><span><input type="checkbox" checked={acknowledge} onChange={(event) => setAcknowledge(event.target.checked)} />I acknowledge pruning, finalizer, namespace, CRD and storage consequences</span></label>}
    <button type="button" disabled={busy || preview.consequences.length > 0 && !acknowledge} onClick={() => onApply(acknowledge)}>Apply GitOps preview</button>
  </section>
}
