import { useState } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { KubernetesContext, KubernetesSecret, KubernetesStatus } from '../types'

const COLUMNS_TEMPLATE = 'minmax(160px,1.2fr) minmax(110px,0.7fr) minmax(150px,1fr) minmax(80px,0.4fr) minmax(90px,0.5fr) minmax(70px,0.4fr)'

export function Secrets({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)

  const status = usePolledResource<KubernetesStatus>(
    (signal) => apiGet(`/api/kubernetes/status?context=${encodeURIComponent(context)}`, signal),
    10000,
    [context],
    `kubernetes:${context}:status`,
  )
  const secrets = usePolledResource<KubernetesSecret[]>(
    (signal) => apiGet(`/api/kubernetes/secrets?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal),
    6000,
    [context, namespace],
    `kubernetes:${context}:secrets:${namespace}`,
  )
  const items = secrets.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((secret) => normalizedQuery === '' || [
    secret.name,
    secret.namespace,
    secret.type,
    ...secret.keys,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const key = (secret: KubernetesSecret) => `${secret.namespace}/${secret.name}`
  const selected = items.find((secret) => key(secret) === selectedKey) ?? null
  const available = !status.loading && !status.error && (status.data?.available ?? false)
  const ready = available && !secrets.loading && !secrets.error

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Secret signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} secret(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter secrets</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter secrets by name, type, or key" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <label className="namespaceField">
          <span>Namespace</span>
          <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} secrets</span>
        <button className="refreshControl" type="button" onClick={secrets.reload}>Refresh</button>
      </div>
      <div className="workArea">
        {!ready ? (
          <RuntimeGate
            label="Kubernetes Secrets"
            enabled={status.data?.enabled ?? false}
            message={secrets.error || status.data?.message || status.error}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={key}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedKey}
            onSelect={(secret) => setSelectedKey(key(secret))}
            ariaLabel="Kubernetes secrets"
            emptyMessage={secrets.error || 'No secrets found in this namespace.'}
            columns={[
              { header: 'Name', render: (secret) => <strong>{secret.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (secret) => secret.namespace },
              { header: 'Type', className: 'mono', render: (secret) => secret.type || 'Opaque' },
              { header: 'Keys', className: 'mono', render: (secret) => secret.keys.length },
              { header: 'Immutable', className: 'mono', render: (secret) => secret.immutable ? 'yes' : 'no' },
              { header: 'Age', className: 'mono', render: (secret) => secret.age },
            ]}
          />
        )}

        {ready && selected && (
          <Inspector title={selected.name} subtitle={`${selected.namespace} · ${selected.type || 'Opaque'}`} onClose={() => setSelectedKey(null)}>
            <section className="drawerPanel">
              <h3>Secret detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Type</dt><dd>{selected.type || 'Opaque'}</dd></div>
                <div><dt>Keys</dt><dd>{selected.keys.length}</dd></div>
                <div><dt>Immutable</dt><dd>{selected.immutable ? 'yes' : 'no'}</dd></div>
                <div><dt>Age</dt><dd>{selected.age}</dd></div>
              </dl>
              <h3>Data keys</h3>
              <p className="hintLine">{selected.keys.join(', ') || 'No data keys are defined.'}</p>
              <p className="hintLine">Secret values are never returned by Porto.</p>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
