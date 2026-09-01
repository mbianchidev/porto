import { useState } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { Inspector, InspectorErrorBoundary } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { KubernetesService, KubernetesStatus } from '../types'

const COLUMNS_TEMPLATE = 'minmax(160px,1.2fr) minmax(110px,0.7fr) minmax(90px,0.6fr) minmax(120px,0.9fr) minmax(150px,1fr) minmax(70px,0.4fr)'

export function KubernetesServices({ context }: { context: string }) {
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)

  const status = usePolledResource<KubernetesStatus>(
    (signal) => apiGet(`/api/kubernetes/status?context=${encodeURIComponent(context)}`, signal),
    10000,
    [context],
  )
  const services = usePolledResource<KubernetesService[]>(
    (signal) => apiGet(`/api/kubernetes/services?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal),
    6000,
    [context, namespace],
  )
  const items = services.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((service) => normalizedQuery === '' || [service.name, service.namespace, service.type]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const key = (service: KubernetesService) => `${service.namespace}/${service.name}`
  const selected = items.find((service) => key(service) === selectedKey) ?? null
  const available = status.data?.available ?? false

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Service signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} service(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter services</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter services by name or type" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <label className="namespaceField">
          <span>Namespace</span>
          <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} services</span>
        <button className="refreshControl" type="button" onClick={services.reload}>Refresh</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Kubernetes" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={key}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedKey}
            onSelect={(service) => setSelectedKey(key(service))}
            ariaLabel="Kubernetes services"
            emptyMessage={services.error || 'No services found in this namespace.'}
            columns={[
              { header: 'Name', render: (service) => <strong>{service.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (service) => service.namespace },
              { header: 'Type', className: 'mono', render: (service) => service.type },
              { header: 'Cluster IP', className: 'mono', render: (service) => service.clusterIP },
              { header: 'Ports', className: 'mono', render: (service) => (service.ports ?? []).map((port) => port.localPort ? `localhost:${port.localPort}` : `${port.port}/${port.protocol}`).join(', ') || '—' },
              { header: 'Age', className: 'mono', render: (service) => service.age },
            ]}
          />
        )}

        {selected && (
          <Inspector title={selected.name} subtitle={`${selected.namespace} · ${selected.type}`} onClose={() => setSelectedKey(null)}>
            <InspectorErrorBoundary key={selectedKey}>
              <section className="drawerPanel">
                <h3>Service detail</h3>
                <dl className="runtimeGrid">
                  <div><dt>Cluster IP</dt><dd>{selected.clusterIP}</dd></div>
                  <div><dt>External IPs</dt><dd>{(selected.externalIPs ?? []).join(', ') || '—'}</dd></div>
                  <div><dt>Age</dt><dd>{selected.age}</dd></div>
                </dl>
                <h3>Ports</h3>
                <dl className="runtimeGrid">
                  {(selected.ports ?? []).map((port) => (
                    <div key={`${port.name}-${port.port}`}>
                      <dt>{port.name || port.protocol}</dt>
                      <dd>
                        {port.port} → {port.targetPort}{port.nodePort ? ` (node ${port.nodePort})` : ''}
                        {port.localPort ? <> · <a href={`http://127.0.0.1:${port.localPort}/`} target="_blank" rel="noreferrer">localhost:{port.localPort}</a></> : ''}
                      </dd>
                    </div>
                  ))}
                </dl>
              </section>
            </InspectorErrorBoundary>
          </Inspector>
        )}
      </div>
    </>
  )
}
