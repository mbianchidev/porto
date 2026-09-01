import { useState } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { KubernetesContext, KubernetesNode, KubernetesStatus } from '../types'

const COLUMNS_TEMPLATE = '12px minmax(160px,1.3fr) minmax(120px,0.8fr) minmax(110px,0.7fr) minmax(120px,0.8fr) minmax(70px,0.4fr)'

export function Nodes({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const [query, setQuery] = useState('')
  const [selectedName, setSelectedName] = useState<string | null>(null)

  const status = usePolledResource<KubernetesStatus>(
    (signal) => apiGet(`/api/kubernetes/status?context=${encodeURIComponent(context)}`, signal),
    10000,
    [context],
    `kubernetes:${context}:status`,
  )
  const nodes = usePolledResource<KubernetesNode[]>(
    (signal) => apiGet(`/api/kubernetes/nodes?context=${encodeURIComponent(context)}`, signal),
    8000,
    [context],
    `kubernetes:${context}:nodes`,
  )
  const items = nodes.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((node) => normalizedQuery === '' || node.name.toLocaleLowerCase().includes(normalizedQuery))
  const selected = items.find((node) => node.name === selectedName) ?? null
  const available = status.data?.available ?? false

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Node signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} node(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter nodes</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter nodes by name" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} nodes</span>
        <button className="refreshControl" type="button" onClick={nodes.reload}>Refresh</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Kubernetes" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(node) => node.name}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(node) => (node.ready ? 'running' : 'crashed')}
            getLampLabel={(node) => (node.ready ? 'ready' : 'not ready')}
            selectedKey={selectedName}
            onSelect={(node) => setSelectedName(node.name)}
            ariaLabel="Kubernetes nodes"
            emptyMessage={nodes.error || 'No nodes found.'}
            columns={[
              { header: 'Name', render: (node) => <strong>{node.name}</strong> },
              { header: 'Roles', className: 'mono', render: (node) => node.roles.join(', ') || 'worker' },
              { header: 'Version', className: 'mono', render: (node) => node.version },
              { header: 'Internal IP', className: 'mono', render: (node) => node.internalIP },
              { header: 'Age', className: 'mono', render: (node) => node.age },
            ]}
          />
        )}

        {selected && (
          <Inspector title={selected.name} subtitle={selected.ready ? 'Ready' : 'Not ready'} onClose={() => setSelectedName(null)}>
            <section className="drawerPanel">
              <h3>Node detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Architecture</dt><dd>{selected.architecture}</dd></div>
                <div><dt>Version</dt><dd>{selected.version}</dd></div>
                <div><dt>Internal IP</dt><dd>{selected.internalIP}</dd></div>
                <div><dt>Unschedulable</dt><dd>{selected.unschedulable ? 'yes' : 'no'}</dd></div>
              </dl>
              <h3>Capacity</h3>
              <dl className="runtimeGrid">
                {Object.entries(selected.capacity).map(([resource, value]) => (
                  <div key={resource}><dt>{resource}</dt><dd>{value}</dd></div>
                ))}
              </dl>
              <h3>Allocatable</h3>
              <dl className="runtimeGrid">
                {Object.entries(selected.allocatable).map(([resource, value]) => (
                  <div key={resource}><dt>{resource}</dt><dd>{value}</dd></div>
                ))}
              </dl>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
