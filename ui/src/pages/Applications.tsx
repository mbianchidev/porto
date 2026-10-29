import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { InventoryList } from '../components/InventoryList'
import { LocalOperations } from '../components/LocalOperations'
import { Inspector } from '../components/Inspector'
import { StatusLamp } from '../components/StatusLamp'
import type { Project } from '../types'
import type { DeliveryApplication, DeliveryPreview, DeliveryRequest, DeliverySource, DeliveryTemplate, LocalOperation } from '../deliveryTypes'

function stringMap(text: string): Record<string, string> {
  const value: unknown = JSON.parse(text)
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some((entry) => typeof entry !== 'string')) throw new Error('Variables must be a JSON object with string values.')
  return value as Record<string, string>
}

const EMPTY_GIT = { repository: '', ref: 'main', path: 'compose.yaml' }
const INITIAL_COMPOSE = 'services:\n  app:\n    image: ${APP_IMAGE:?Set an explicit image reference}\n'

export function Applications() {
  const { notifyError, notifyNotice } = useMessages()
  const applications = usePolledResource<DeliveryApplication[]>((signal) => apiGet('/api/delivery', signal), 3000, [], 'delivery')
  const projects = usePolledResource<Project[]>((signal) => apiGet('/api/projects', signal), 3000, [], 'projects')
  const templates = usePolledResource<DeliveryTemplate[]>((signal) => apiGet('/api/delivery/templates', signal), 0, [])
  const [projectID, setProjectID] = useState(0)
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState('')
  const [sourceKind, setSourceKind] = useState<DeliverySource['kind']>('editor')
  const [git, setGit] = useState(EMPTY_GIT)
  const [compose, setCompose] = useState(INITIAL_COMPOSE)
  const [variables, setVariables] = useState('{"APP_IMAGE": ""}')
  const [secrets, setSecrets] = useState('{}')
  const [gitToken, setGitToken] = useState('')
  const [publicGit, setPublicGit] = useState(false)
  const [pollSeconds, setPollSeconds] = useState(0)
  const [webhookEnabled, setWebhookEnabled] = useState(false)
  const [webhookSecret, setWebhookSecret] = useState('')
  const [detachGit, setDetachGit] = useState(false)
  const [confirmDrift, setConfirmDrift] = useState(false)
  const [templateKey, setTemplateKey] = useState('http-application:1')
  const [templateName, setTemplateName] = useState('')
  const [templateVersion, setTemplateVersion] = useState(1)
  const [preview, setPreview] = useState<DeliveryPreview | null>(null)
  const [busy, setBusy] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const inspectorTrigger = useRef<HTMLElement | null>(null)
  const selected = (applications.data ?? []).find((application) => application.projectId === projectID)
  const selectedTemplate = (templates.data ?? []).find((template) => `${template.name}:${template.version}` === templateKey)
  const project = (projects.data ?? []).find((entry) => entry.id === projectID)

  function select(application?: DeliveryApplication) {
    if (document.activeElement instanceof HTMLElement) inspectorTrigger.current = document.activeElement
    setProjectID(application?.projectId ?? 0)
    setName(application?.name ?? '')
    setSourceKind(application?.source.kind ?? 'editor')
    setGit(application?.source.git ?? EMPTY_GIT)
    setPollSeconds(application?.pollSeconds ?? 0)
    setWebhookEnabled(application?.webhookEnabled ?? false)
    setCompose(application ? '' : INITIAL_COMPOSE)
    setVariables(application ? '{}' : '{"APP_IMAGE": ""}')
    setSecrets('{}')
    setGitToken('')
    setPublicGit(false)
    setWebhookSecret('')
    setDetachGit(false)
    setConfirmDrift(false)
    setPreview(null)
    setEditing(true)
  }

  function closeInspector() {
    setEditing(false)
    requestAnimationFrame(() => { if (inspectorTrigger.current?.isConnected) inspectorTrigger.current.focus() })
  }

  async function run(action: () => Promise<void>) {
    setBusy(true)
    try { await action() } catch (error) { notifyError('delivery', errorMessage(error, 'Application delivery failed')) } finally { setBusy(false) }
  }

  function prepare() {
    void run(async () => {
      const source: DeliverySource = { kind: sourceKind, git: sourceKind === 'git' ? git : EMPTY_GIT }
      if (sourceKind === 'template' && selectedTemplate) { source.template = selectedTemplate.name; source.templateVersion = selectedTemplate.version }
      const request: DeliveryRequest = {
        projectId: projectID, name, target: 'local', source, compose,
        variables: stringMap(variables), secrets: stringMap(secrets),
        gitToken: publicGit ? '' : gitToken || undefined, pollSeconds: sourceKind === 'git' ? pollSeconds : 0,
        webhookEnabled: sourceKind === 'git' && webhookEnabled, webhookSecret: webhookSecret || undefined, detachGit, confirmDrift,
      }
      const result = await apiSend<DeliveryPreview>('/api/delivery/previews', 'POST', request)
      setProjectID(result.projectId)
      setPreview(result)
      setSecrets('{}')
      setGitToken('')
      setWebhookSecret('')
      applications.reload()
      projects.reload()
    })
  }

  function apply() {
    if (!preview || !window.confirm('Apply this preview to the selected local Compose project? Services may restart. Persistent volumes are retained; database schemas and application data are not rolled back.')) return
    void run(async () => {
      await apiSend<LocalOperation>(`/api/delivery/${preview.projectId}/deployments`, 'POST', { preview: preview.token, confirm: true })
      setPreview(null)
      applications.reload()
      notifyNotice('delivery', 'Application operation queued. Follow its health and failure state below.')
    })
  }

  async function loadDefinition() {
    if (!window.confirm('Reveal the saved Compose source for editing? It may contain inline sensitive values. Stored secret-variable values are not revealed.')) return
    await run(async () => {
      const result = await apiGet<DeliveryRequest>(`/api/delivery/${projectID}/definition?reveal=true`)
      setCompose(result.compose)
      setVariables(JSON.stringify(result.variables ?? {}, null, 2))
      setPreview(null)
    })
  }

  return <>
    <section className="fleetRail" aria-label="Application delivery status">
      <span className="fleetRailTitle">Local application delivery</span>
      <span className="fleetDatum">{applications.data?.length ?? 0} recorded application(s)</span>
      <span className="fleetMessage">Existing projects and Compose ownership. Automatic updates are opt-in.</span>
    </section>
    <div className="controlBar">
      <button type="button" onClick={() => select()}>Create application</button>
      <button type="button" onClick={() => { applications.reload(); projects.reload() }}>Refresh applications</button>
    </div>
    <div className="workArea">
      {applications.error && <p role="alert">{applications.error}</p>}
      <InventoryList items={applications.data ?? []} getKey={(application) => String(application.projectId)} selectedKey={String(projectID)} onSelect={select} columnsTemplate="minmax(160px,1fr) minmax(100px,.7fr) minmax(150px,1fr)" ariaLabel="Recorded applications" emptyMessage="No applications recorded. Create one or adopt an existing Compose project." columns={[
        { header: 'Application', render: (application) => <strong>{application.name}</strong> },
        { header: 'Ownership', render: (application) => `${application.source.kind} / ${application.target}` },
        { header: 'State', render: (application) => <><StatusLamp state={application.lastError || application.drift ? 'crashed' : application.currentRevision ? 'running' : 'stopped'} />{application.drift ? 'Definition drift' : application.lastError ? 'Needs attention' : application.currentRevision ? 'Revision recorded' : 'Not deployed'}</> },
      ]} />
      {editing && <Inspector title={selected?.name || 'Application definition'} subtitle={project ? `Project ${project.id} / ${project.status} / port ${project.port || 'not assigned'}` : 'Local Linux Compose target'} onClose={closeInspector}>
        <section className="drawerPanel">
          <h3>Source and configuration</h3>
          <p>Compose services use the existing project controller and stable loopback port plan. Preview hides environment values; deployment logs redact stored secret-variable values.</p>
          <form className="inspectorForm" onChange={() => setPreview(null)} onSubmit={(event) => { event.preventDefault(); prepare() }}>
            {!selected && <label>Existing Compose project<select value={projectID} onChange={(event) => {
              const id = Number(event.target.value)
              setProjectID(id)
              const candidate = (projects.data ?? []).find((entry) => entry.id === id)
              if (candidate) setName(candidate.name)
              setPreview(null)
            }}><option value="0">Create a new project</option>{(projects.data ?? []).filter((entry) => entry.strategy === 'compose').map((entry) => <option key={entry.id} value={entry.id}>{entry.name} / {entry.path}</option>)}</select></label>}
            {!projectID && <label>Application name<input required pattern="[A-Za-z0-9][A-Za-z0-9_-]{0,62}" value={name} onChange={(event) => setName(event.target.value)} /></label>}
            <label>Source<select value={sourceKind} onChange={(event) => setSourceKind(event.target.value as DeliverySource['kind'])}>
              <option value="editor">Compose editor</option><option value="upload">Compose file upload</option><option value="template">Versioned template</option><option value="git">Git repository</option>
            </select></label>
            {sourceKind === 'git' && <>
              <label>Repository HTTPS URL or selected local repository<input required value={git.repository} onChange={(event) => setGit({ ...git, repository: event.target.value })} /></label>
              <label>Explicit branch, tag or commit<input required value={git.ref} onChange={(event) => setGit({ ...git, ref: event.target.value })} /></label>
              <label>Repository-relative Compose file<input required value={git.path} onChange={(event) => setGit({ ...git, path: event.target.value })} /></label>
              <label>Scoped private-repository token<input type="password" autoComplete="new-password" value={gitToken} onChange={(event) => setGitToken(event.target.value)} placeholder="Leave blank to retain stored access" /></label>
              <label><input type="checkbox" checked={publicGit} onChange={(event) => setPublicGit(event.target.checked)} />Use public access, clearing this source's stored token after deployment</label>
              <label>Opt-in polling interval (seconds, 0 disables)<input type="number" min="0" max="86400" value={pollSeconds} onChange={(event) => setPollSeconds(Number(event.target.value))} /></label>
              <label><input type="checkbox" checked={webhookEnabled} onChange={(event) => setWebhookEnabled(event.target.checked)} />Enable authenticated webhook updates</label>
              <label>Webhook signing secret (32+ characters)<input type="password" autoComplete="new-password" value={webhookSecret} onChange={(event) => setWebhookSecret(event.target.value)} placeholder="Leave blank to retain stored signing secret" /></label>
            </>}
            {sourceKind === 'template' && <>
              <label>Template version<select value={templateKey} onChange={(event) => setTemplateKey(event.target.value)}>{(templates.data ?? []).map((template) => <option key={`${template.name}:${template.version}`} value={`${template.name}:${template.version}`}>{template.name} v{template.version}</option>)}</select></label>
              <p>{selectedTemplate?.description} Required: {selectedTemplate?.required.join(', ')}. Secrets: {selectedTemplate?.secretNames.join(', ') || 'none'}.</p>
              <pre className="definitionText">{selectedTemplate?.compose}</pre>
              <button type="button" disabled={!selectedTemplate} onClick={() => { setCompose(selectedTemplate?.compose ?? ''); setSourceKind('editor'); setPreview(null) }}>Make an editable copy</button>
            </>}
            {sourceKind === 'upload' && <>
              <button type="button" onClick={() => fileInput.current?.click()}>Choose Compose file</button>
              <input ref={fileInput} className="visuallyHidden" type="file" accept=".yaml,.yml,.json" aria-label="Compose file" onChange={(event) => {
                const file = event.target.files?.[0]
                event.target.value = ''
                if (!file) return
                void run(async () => {
                  if (file.size > 1024 * 1024) throw new Error('Compose files must be at most 1 MiB.')
                  setCompose(await file.text())
                  setPreview(null)
                })
              }} />
            </>}
            {(sourceKind === 'editor' || sourceKind === 'upload') && <>
              {projectID > 0 && <button type="button" disabled={busy} onClick={() => void loadDefinition()}>Reveal saved Compose source</button>}
              <label>Compose YAML<textarea rows={16} value={compose} onChange={(event) => setCompose(event.target.value)} spellCheck={false} aria-label="Compose YAML editor" /></label>
            </>}
            {selected?.source.kind === 'git' && sourceKind !== 'git' && <label><input type="checkbox" checked={detachGit} onChange={(event) => setDetachGit(event.target.checked)} />Detach Git ownership and disable automatic updates before applying this edit</label>}
            {selected?.drift && <label><input type="checkbox" checked={confirmDrift} onChange={(event) => setConfirmDrift(event.target.checked)} />Replace the externally changed or partially applied definition after this preview</label>}
            <label>Non-secret variables (JSON)<textarea rows={4} value={variables} onChange={(event) => setVariables(event.target.value)} spellCheck={false} /></label>
            <label>Secret variables (JSON, omitted keys keep stored values)<input type="password" autoComplete="new-password" value={secrets} onChange={(event) => setSecrets(event.target.value)} /></label>
            <button type="submit" disabled={busy}>{busy ? 'Preparing operation...' : 'Validate and preview'}</button>
          </form>
          {sourceKind === 'editor' && <details><summary>Save a reusable template</summary>
            <form className="inspectorForm" onSubmit={(event) => {
              event.preventDefault()
              void run(async () => {
                await apiSend('/api/delivery/templates', 'POST', { name: templateName, version: templateVersion, compose, description: 'User-owned Compose template. Supply sensitive values through secret variables.', required: [], secretNames: [] })
                templates.reload()
                notifyNotice('delivery', 'Template version saved. Use variables rather than embedding secrets.')
              })
            }}>
              <label>Template name<input required pattern="[a-z][a-z0-9-]{0,63}" value={templateName} onChange={(event) => setTemplateName(event.target.value)} /></label>
              <label>New version<input type="number" min="1" max="10000" required value={templateVersion} onChange={(event) => setTemplateVersion(Number(event.target.value))} /></label>
              <button type="submit" disabled={busy || !compose}>Save template version</button>
            </form>
          </details>}
        </section>
        {preview && <section className="drawerPanel" aria-label="Application deployment preview">
          <h3>Configuration preview</h3>
          <p>Selected Git commit: <span className="mono">{preview.commit || 'Not a Git source'}</span>. Expires {preview.expiresAt}.</p>
          {preview.warnings.map((warning) => <p role="status" key={warning}>{warning}</p>)}
          <h4>Before</h4><pre className="definitionText">{preview.before}</pre><h4>After</h4><pre className="definitionText">{preview.after}</pre>
          <button type="button" disabled={busy} onClick={apply}>Apply preview</button>
        </section>}
        {selected && <section className="drawerPanel">
          <h3>Recorded deployments</h3>
          <div className="actions">
            <a href="#/localhost-ing">Open project controls</a>
            <button type="button" disabled={busy || !selected.currentRevision || selected.drift || project?.status === 'running' || project?.status === 'starting'} onClick={() => void run(async () => {
              await apiSend(`/api/projects/${selected.projectId}/start?noPull=true`, 'POST')
              projects.reload()
              notifyNotice('delivery', 'Application start requested. Readiness is checked against its Compose services.')
            })}>Start application</button>
            <button type="button" disabled={busy} onClick={() => void run(async () => {
              await apiSend(`/api/projects/${selected.projectId}/stop`, 'POST')
              projects.reload()
              notifyNotice('delivery', 'Application stopped. Persistent volumes are retained.')
            })}>Stop application</button>
          </div>
          {selected.lastError && <p role="alert">{selected.lastError}</p>}
          {selected.drift && <p role="alert">The live definition differs from the last successful revision. Automatic reconciliation will not overwrite it.</p>}
          {selected.webhookEnabled && <p>Authenticated webhook-only listener: <span className="mono">{selected.webhookUrl || 'Unavailable'}</span>. Never expose Porto's local control API.</p>}
          <p>Polling: {selected.pollSeconds ? `${selected.pollSeconds}s, next ${selected.nextPollAt}` : 'disabled'}. Failed attempts: {selected.failures}.</p>
          {selected.source.kind === 'git' && <div className="actions">
            <button type="button" disabled={busy} onClick={() => void run(async () => { await apiSend(`/api/delivery/${selected.projectId}/updates`, 'POST'); applications.reload(); notifyNotice('delivery', 'Git update check queued.') })}>Check Git updates</button>
            <button type="button" disabled={busy} onClick={() => void run(async () => {
              await apiSend(`/api/delivery/${selected.projectId}/automation`, 'PUT', { pollSeconds: 0, webhookEnabled: false })
              setPollSeconds(0); setWebhookEnabled(false); applications.reload()
              notifyNotice('delivery', 'Automatic application updates paused.')
            })}>Pause automatic updates</button>
          </div>}
          {[...selected.revisions].reverse().map((revision) => <details key={revision.id}><summary>{revision.createdAt} / {revision.commit?.slice(0, 12) || revision.id.slice(0, 12)}{revision.id === selected.currentRevision ? ' / current' : ''}{revision.rollbackOf ? ' / rollback' : ''}</summary>
            <p className="mono">Configuration: {revision.configDigest}</p>
            <p className="mono">Effective configuration: {revision.effectiveDigest}</p>
            <pre className="definitionText">{JSON.stringify(revision.images, null, 2)}</pre>
            {revision.warnings?.map((warning) => <p role="status" key={warning}>{warning}</p>)}
            <button type="button" disabled={busy} onClick={() => void run(async () => {
              const result = await apiSend<DeliveryPreview>(`/api/delivery/${selected.projectId}/rollback-previews`, 'POST', { revision: revision.id })
              setPreview(result)
            })}>Preview rollback</button>
          </details>)}
        </section>}
        {projectID > 0 && <LocalOperations resource={`project:${projectID}`} />}
      </Inspector>}
    </div>
  </>
}
