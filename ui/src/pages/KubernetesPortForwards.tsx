import { useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { formatRelativeTime } from '../format'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesContext,
  KubernetesPortForward,
  KubernetesPortForwardResource,
} from '../types'

type ForwardDraft = {
  namespace: string
  resourceType: KubernetesPortForwardResource
  resourceName: string
  localPort: string
  remotePort: string
}

const EMPTY_DRAFT: ForwardDraft = {
  namespace: 'default',
  resourceType: 'service',
  resourceName: '',
  localPort: '',
  remotePort: '80',
}

function sortForwards(forwards: KubernetesPortForward[]) {
  return [...forwards].sort((left, right) => left.localPort - right.localPort || left.id.localeCompare(right.id))
}

export function KubernetesPortForwards({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [draft, setDraft] = useState<ForwardDraft>(EMPTY_DRAFT)
  const [submitting, setSubmitting] = useState(false)
  const [stoppingID, setStoppingID] = useState<string | null>(null)
  const [selectedID, setSelectedID] = useState<string | null>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const createToggleRef = useRef<HTMLButtonElement | null>(null)
  const status = useKubernetesStatus(context)
  const available = !status.loading && !status.error && (status.data?.available ?? false)
  const forwards = usePolledResource<KubernetesPortForward[]>(
    (signal) => available
      ? apiGet(`/api/kubernetes/port-forwards?context=${encodeURIComponent(context)}`, signal)
      : Promise.resolve([]),
    3000,
    [context, available],
    available ? `kubernetes:${context}:port-forwards` : undefined,
  )
  const items = forwards.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((forward) => normalizedQuery === '' || [
    forward.resourceName,
    forward.resourceType,
    forward.namespace,
    `${forward.address}:${forward.localPort}`,
    String(forward.remotePort),
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const selected = items.find((forward) => forward.id === selectedID) ?? null
  const loading = status.loading || (available && forwards.loading)
  const ready = available && !forwards.loading && !forwards.error

  function updateDraft<K extends keyof ForwardDraft>(key: K, value: ForwardDraft[K]) {
    setDraft((current) => ({ ...current, [key]: value }))
  }

  async function startForward(event: FormEvent) {
    event.preventDefault()
    if (submitting) return
    const resourceName = draft.resourceName.trim()
    const namespace = draft.namespace.trim()
    const localPort = draft.localPort === '' ? 0 : Number(draft.localPort)
    const remotePort = Number(draft.remotePort)
    if (!resourceName || !namespace || !Number.isInteger(localPort) || !Number.isInteger(remotePort)) {
      notifyError('port-forwards', 'Namespace, resource name, and valid numeric ports are required.')
      return
    }
    setSubmitting(true)
    try {
      const result = await apiSend<KubernetesPortForward>(
        `/api/kubernetes/port-forwards?context=${encodeURIComponent(context)}`,
        'POST',
        {
          namespace,
          resourceType: draft.resourceType,
          resourceName,
          localPort,
          remotePort,
        },
      )
      forwards.update((current) => sortForwards([...(current ?? []).filter((item) => item.id !== result.id), result]))
      setDraft((current) => ({ ...EMPTY_DRAFT, resourceType: current.resourceType }))
      setCreateOpen(false)
      returnFocusRef.current = createToggleRef.current
      setSelectedID(result.id)
      notifyNotice('port-forwards', `Forwarding 127.0.0.1:${result.localPort} to ${result.resourceType}/${result.resourceName}:${result.remotePort}.`)
    } catch (err) {
      notifyError('port-forwards', errorMessage(err, 'Unable to start Kubernetes port forward'))
    } finally {
      setSubmitting(false)
    }
  }

  async function stopForward(forward: KubernetesPortForward) {
    if (stoppingID) return
    setStoppingID(forward.id)
    try {
      await apiSend(
        `/api/kubernetes/port-forwards/${encodeURIComponent(forward.id)}?context=${encodeURIComponent(context)}`,
        'DELETE',
      )
      forwards.update((current) => current?.filter((item) => item.id !== forward.id) ?? [])
      if (selectedID === forward.id) setSelectedID(null)
      notifyNotice('port-forwards', `Stopped forward on 127.0.0.1:${forward.localPort}.`)
    } catch (err) {
      notifyError('port-forwards', errorMessage(err, `Unable to stop forward on port ${forward.localPort}`))
    } finally {
      setStoppingID(null)
    }
  }

  function selectForward(forward: KubernetesPortForward) {
    returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectedID(forward.id)
  }

  function closeInspector() {
    setSelectedID(null)
    window.requestAnimationFrame(() => returnFocusRef.current?.focus())
  }

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes port forwarding status">
        <span className="fleetRailTitle">Forwarding signal</span>
        <span className="fleetDatum">
          <StatusLamp state={status.loading ? 'neutral' : available ? 'running' : 'crashed'} />
          {status.loading ? 'Checking' : available ? 'Available' : 'Unavailable'}
        </span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} active forward(s)</span>
      </section>
      <div className="controlBar kubernetesResourceControlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter port forwards</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter forwards by target or port" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} forwards</span>
        <button
          ref={createToggleRef}
          type="button"
          disabled={!available}
          onClick={() => setCreateOpen((value) => !value)}
          aria-expanded={createOpen}
          aria-controls="port-forward-create"
        >
          {createOpen ? 'Close' : 'New forward'}
        </button>
        <button className="refreshControl" type="button" onClick={() => { status.reload(); forwards.reload() }}>Refresh</button>
      </div>

      <section className="drawerPanel portForwardPanel" id="port-forward-create" hidden={!createOpen}>
        <div className="drawerPanelHeading">
          <div>
            <h3>Start port forward</h3>
            <p>Bind a local loopback port to a Service, Pod, or Deployment in the selected context.</p>
          </div>
        </div>
        <form className="inspectorForm portForwardForm" onSubmit={startForward}>
          <label>
            Namespace
            <input type="text" value={draft.namespace} required onChange={(event) => updateDraft('namespace', event.target.value)} />
          </label>
          <label>
            Target type
            <select value={draft.resourceType} onChange={(event) => updateDraft('resourceType', event.target.value as KubernetesPortForwardResource)}>
              <option value="service">Service</option>
              <option value="pod">Pod</option>
              <option value="deployment">Deployment</option>
            </select>
          </label>
          <label>
            Resource name
            <input type="text" value={draft.resourceName} required placeholder="api" onChange={(event) => updateDraft('resourceName', event.target.value)} />
          </label>
          <label>
            Local port
            <input type="number" min="1" max="65535" value={draft.localPort} placeholder="automatic" onChange={(event) => updateDraft('localPort', event.target.value)} />
          </label>
          <label>
            Remote port
            <input type="number" min="1" max="65535" value={draft.remotePort} required onChange={(event) => updateDraft('remotePort', event.target.value)} />
          </label>
          <div className="portForwardActions">
            <button type="submit" disabled={submitting}>{submitting ? 'Starting...' : 'Start forward'}</button>
            <button type="button" disabled={submitting} onClick={() => { setDraft(EMPTY_DRAFT); setCreateOpen(false) }}>Cancel</button>
          </div>
        </form>
      </section>

      <div className="workArea">
        {loading ? (
          <article className="empty" role="status"><p>Loading port forwards...</p></article>
        ) : !ready ? (
          <RuntimeGate
            label="Kubernetes port forwarding"
            enabled={status.data?.enabled ?? false}
            message={forwards.error || status.data?.message || status.error}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(forward) => forward.id}
            columnsTemplate="12px minmax(180px,1.2fr) minmax(110px,0.7fr) minmax(130px,0.8fr) minmax(90px,0.5fr) minmax(100px,0.6fr)"
            getLamp={() => 'running'}
            getLampLabel={() => 'running'}
            selectedKey={selectedID}
            onSelect={selectForward}
            renderActions={(forward) => (
              <ActionButton
                label={`Stop forward on port ${forward.localPort}`}
                icon="stop"
                disabled={stoppingID !== null}
                onClick={() => void stopForward(forward)}
              />
            )}
            ariaLabel="Kubernetes port forwards"
            emptyMessage={forwards.error || 'No port forwards are running. Start one to expose a workload on localhost.'}
            columns={[
              { header: 'Target', render: (forward) => <strong>{forward.resourceType}/{forward.resourceName}</strong> },
              { header: 'Namespace', className: 'mono', render: (forward) => forward.namespace },
              { header: 'Local endpoint', className: 'mono', render: (forward) => `${forward.address}:${forward.localPort}` },
              { header: 'Remote port', className: 'mono', render: (forward) => forward.remotePort },
              { header: 'Started', className: 'mono', render: (forward) => formatRelativeTime(forward.startedAt) },
            ]}
          />
        )}

        {ready && selected && (
          <Inspector title={`${selected.address}:${selected.localPort}`} subtitle={`${selected.resourceType}/${selected.resourceName}`} onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>Port forward detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Status</dt><dd>{selected.status}</dd></div>
                <div><dt>Context</dt><dd>{selected.context}</dd></div>
                <div><dt>Namespace</dt><dd>{selected.namespace}</dd></div>
                <div><dt>Target</dt><dd>{selected.resourceType}/{selected.resourceName}</dd></div>
                <div><dt>Local endpoint</dt><dd>{selected.address}:{selected.localPort}</dd></div>
                <div><dt>Remote port</dt><dd>{selected.remotePort}</dd></div>
                <div><dt>Started</dt><dd>{formatRelativeTime(selected.startedAt)}</dd></div>
              </dl>
              <div className="maintenanceBar">
                <button type="button" disabled={stoppingID !== null} onClick={() => void stopForward(selected)}>
                  {stoppingID === selected.id ? 'Stopping...' : 'Stop forward'}
                </button>
              </div>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
