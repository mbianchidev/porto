import { useRef, useState } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { KubernetesContextSelect } from '../components/KubernetesContextSelect'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesContext,
  KubernetesGateway,
  KubernetesGatewayClass,
  KubernetesHTTPRoute,
} from '../types'

type GatewayTab = 'classes' | 'gateways' | 'routes'

function conditionLamp(status: string): 'running' | 'starting' | 'crashed' | 'neutral' {
  switch (status.toLocaleLowerCase()) {
    case 'true':
      return 'running'
    case 'false':
      return 'crashed'
    case 'unknown':
      return 'starting'
    default:
      return 'neutral'
  }
}

export function KubernetesGateways({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const [tab, setTab] = useState<GatewayTab>('gateways')
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const status = useKubernetesStatus(context)
  const available = status.data?.available ?? false
  const classes = usePolledResource<KubernetesGatewayClass[]>(
    (signal) => available && tab === 'classes'
      ? apiGet(`/api/kubernetes/gateway-classes?context=${encodeURIComponent(context)}`, signal)
      : Promise.resolve([]),
    8000,
    [context, available, tab],
    available && tab === 'classes' ? `kubernetes:${context}:gateway-classes` : undefined,
  )
  const gateways = usePolledResource<KubernetesGateway[]>(
    (signal) => available && tab === 'gateways'
      ? apiGet(`/api/kubernetes/gateways?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    8000,
    [context, namespace, available, tab],
    available && tab === 'gateways' ? `kubernetes:${context}:gateways:${namespace}` : undefined,
  )
  const routes = usePolledResource<KubernetesHTTPRoute[]>(
    (signal) => available && tab === 'routes'
      ? apiGet(`/api/kubernetes/http-routes?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    8000,
    [context, namespace, available, tab],
    available && tab === 'routes' ? `kubernetes:${context}:http-routes:${namespace}` : undefined,
  )
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const classItems = classes.data ?? []
  const gatewayItems = (gateways.data ?? []).map((item) => ({
    ...item,
    addresses: item.addresses ?? [],
    listeners: item.listeners ?? [],
  }))
  const routeItems = (routes.data ?? []).map((item) => ({
    ...item,
    hostnames: item.hostnames ?? [],
    parentRefs: item.parentRefs ?? [],
    backendRefs: item.backendRefs ?? [],
    parents: item.parents ?? [],
  }))
  const filteredClasses = classItems.filter((item) => normalizedQuery === '' || [
    item.name,
    item.controllerName,
    item.accepted,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const filteredGateways = gatewayItems.filter((item) => normalizedQuery === '' || [
    item.name,
    item.namespace,
    item.className,
    ...item.addresses,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const filteredRoutes = routeItems.filter((item) => normalizedQuery === '' || [
    item.name,
    item.namespace,
    ...item.hostnames,
    ...item.parentRefs,
    ...item.backendRefs,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const namespacedKey = (item: { namespace: string; name: string }) => `${item.namespace}/${item.name}`
  const selectedClass = tab === 'classes' ? classItems.find((item) => item.name === selectedKey) ?? null : null
  const selectedGateway = tab === 'gateways' ? gatewayItems.find((item) => namespacedKey(item) === selectedKey) ?? null : null
  const selectedRoute = tab === 'routes' ? routeItems.find((item) => namespacedKey(item) === selectedKey) ?? null : null
  const activeItems = tab === 'classes' ? classItems : tab === 'gateways' ? gatewayItems : routeItems
  const filteredCount = tab === 'classes' ? filteredClasses.length : tab === 'gateways' ? filteredGateways.length : filteredRoutes.length
  const activeError = tab === 'classes' ? classes.error : tab === 'gateways' ? gateways.error : routes.error

  function selectTab(id: string) {
    setTab(id as GatewayTab)
    setSelectedKey(null)
  }

  function selectResource(key: string) {
    returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectedKey(key)
  }

  function closeInspector() {
    setSelectedKey(null)
    window.requestAnimationFrame(() => returnFocusRef.current?.focus())
  }

  return (
    <>
      <section className="fleetRail" aria-label="Gateway API status">
        <span className="fleetRailTitle">Gateway signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{activeItems.length} {tab === 'classes' ? 'GatewayClass(es)' : tab === 'gateways' ? 'Gateway(s)' : 'HTTPRoute(s)'}</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter Gateway API resources</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter Gateway API resources" onChange={(event) => setQuery(event.target.value)} />
        </label>
        {tab !== 'classes' && (
          <label className="namespaceField">
            <span>Namespace</span>
            <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
          </label>
        )}
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filteredCount} / {activeItems.length}</span>
        <button className="refreshControl" type="button" onClick={() => {
          status.reload()
          if (tab === 'classes') classes.reload()
          else if (tab === 'gateways') gateways.reload()
          else routes.reload()
        }}>Refresh</button>
      </div>
      <InspectorTabs
        tabs={[
          { id: 'classes', label: 'Gateway classes' },
          { id: 'gateways', label: 'Gateways' },
          { id: 'routes', label: 'HTTP routes' },
        ]}
        activeID={tab}
        onSelect={selectTab}
      />
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Gateway API" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : tab === 'classes' ? (
          <InventoryList
            items={filteredClasses}
            getKey={(item) => item.name}
            columnsTemplate="12px minmax(150px,1fr) minmax(260px,1.5fr) minmax(90px,0.5fr) minmax(120px,0.8fr) minmax(70px,0.4fr)"
            getLamp={(item) => conditionLamp(item.accepted)}
            getLampLabel={(item) => `Accepted ${item.accepted}`}
            selectedKey={selectedKey}
            onSelect={(item) => selectResource(item.name)}
            ariaLabel="Kubernetes GatewayClasses"
            emptyMessage={activeError || 'No GatewayClasses found. The Gateway API CRDs may not be installed.'}
            columns={[
              { header: 'Name', render: (item) => <strong>{item.name}</strong> },
              { header: 'Controller', className: 'mono', render: (item) => item.controllerName },
              { header: 'Accepted', className: 'mono', render: (item) => item.accepted },
              { header: 'Reason', className: 'mono', render: (item) => item.reason || '—' },
              { header: 'Age', className: 'mono', render: (item) => item.age },
            ]}
          />
        ) : tab === 'gateways' ? (
          <InventoryList
            items={filteredGateways}
            getKey={namespacedKey}
            columnsTemplate="12px minmax(140px,1fr) minmax(110px,0.7fr) minmax(100px,0.6fr) minmax(90px,0.5fr) minmax(160px,1fr) minmax(70px,0.4fr)"
            getLamp={(item) => conditionLamp(item.programmed)}
            getLampLabel={(item) => `Programmed ${item.programmed}`}
            selectedKey={selectedKey}
            onSelect={(item) => selectResource(namespacedKey(item))}
            ariaLabel="Kubernetes Gateways"
            emptyMessage={activeError || 'No Gateways found.'}
            columns={[
              { header: 'Name', render: (item) => <strong>{item.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (item) => item.namespace },
              { header: 'Class', className: 'mono', render: (item) => item.className },
              { header: 'Programmed', className: 'mono', render: (item) => item.programmed },
              { header: 'Addresses', className: 'mono', render: (item) => item.addresses.join(', ') || '—' },
              { header: 'Age', className: 'mono', render: (item) => item.age },
            ]}
          />
        ) : (
          <InventoryList
            items={filteredRoutes}
            getKey={namespacedKey}
            columnsTemplate="12px minmax(140px,1fr) minmax(110px,0.7fr) minmax(180px,1.1fr) minmax(100px,0.6fr) minmax(170px,1fr) minmax(70px,0.4fr)"
            getLamp={(item) => conditionLamp(item.accepted)}
            getLampLabel={(item) => `Accepted ${item.accepted}`}
            selectedKey={selectedKey}
            onSelect={(item) => selectResource(namespacedKey(item))}
            ariaLabel="Kubernetes HTTPRoutes"
            emptyMessage={activeError || 'No HTTPRoutes found.'}
            columns={[
              { header: 'Name', render: (item) => <strong>{item.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (item) => item.namespace },
              { header: 'Hostnames', className: 'mono', render: (item) => item.hostnames.join(', ') || '—' },
              { header: 'Accepted', className: 'mono', render: (item) => item.accepted },
              { header: 'Backends', className: 'mono', render: (item) => item.backendRefs.join(', ') || '—' },
              { header: 'Age', className: 'mono', render: (item) => item.age },
            ]}
          />
        )}

        {selectedClass && (
          <Inspector title={selectedClass.name} subtitle="GatewayClass" onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>Gateway class detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Controller</dt><dd>{selectedClass.controllerName}</dd></div>
                <div><dt>Accepted</dt><dd>{selectedClass.accepted}</dd></div>
                <div><dt>Reason</dt><dd>{selectedClass.reason || '—'}</dd></div>
                <div><dt>Message</dt><dd>{selectedClass.message || '—'}</dd></div>
                <div><dt>Age</dt><dd>{selectedClass.age}</dd></div>
              </dl>
            </section>
          </Inspector>
        )}
        {selectedGateway && (
          <Inspector title={selectedGateway.name} subtitle={`${selectedGateway.namespace} · Gateway`} onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>Gateway detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Class</dt><dd>{selectedGateway.className}</dd></div>
                <div><dt>Accepted</dt><dd>{selectedGateway.accepted}</dd></div>
                <div><dt>Accepted detail</dt><dd>{selectedGateway.acceptedReason || '—'}{selectedGateway.acceptedMessage ? ` · ${selectedGateway.acceptedMessage}` : ''}</dd></div>
                <div><dt>Programmed</dt><dd>{selectedGateway.programmed}</dd></div>
                <div><dt>Programmed detail</dt><dd>{selectedGateway.programmedReason || '—'}{selectedGateway.programmedMessage ? ` · ${selectedGateway.programmedMessage}` : ''}</dd></div>
                <div><dt>Addresses</dt><dd>{selectedGateway.addresses.join(', ') || '—'}</dd></div>
                <div><dt>Age</dt><dd>{selectedGateway.age}</dd></div>
              </dl>
              <h3>Listeners</h3>
              <dl className="runtimeGrid">
                {selectedGateway.listeners.map((listener) => (
                  <div key={listener.name}>
                    <dt>{listener.name} · {listener.protocol}:{listener.port}</dt>
                    <dd>
                      {listener.hostname || '*'} · {listener.attachedRoutes} route(s) · accepted {listener.accepted} · programmed {listener.programmed}
                      {listener.acceptedReason ? ` · accepted: ${listener.acceptedReason}` : ''}
                      {listener.programmedReason ? ` · programmed: ${listener.programmedReason}` : ''}
                      {listener.acceptedMessage ? ` · ${listener.acceptedMessage}` : ''}
                      {listener.programmedMessage ? ` · ${listener.programmedMessage}` : ''}
                    </dd>
                  </div>
                ))}
              </dl>
            </section>
          </Inspector>
        )}
        {selectedRoute && (
          <Inspector title={selectedRoute.name} subtitle={`${selectedRoute.namespace} · HTTPRoute`} onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>HTTP route detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Hostnames</dt><dd>{selectedRoute.hostnames.join(', ') || '—'}</dd></div>
                <div><dt>Parents</dt><dd>{selectedRoute.parentRefs.join(', ') || '—'}</dd></div>
                <div><dt>Backends</dt><dd>{selectedRoute.backendRefs.join(', ') || '—'}</dd></div>
                <div><dt>Accepted</dt><dd>{selectedRoute.accepted}</dd></div>
                <div><dt>Resolved refs</dt><dd>{selectedRoute.resolvedRefs}</dd></div>
                <div><dt>Age</dt><dd>{selectedRoute.age}</dd></div>
              </dl>
              <h3>Parent status</h3>
              <dl className="runtimeGrid">
                {selectedRoute.parents.map((parent, index) => (
                  <div key={`${parent.parentRef}:${parent.controllerName}:${index}`}>
                    <dt>{parent.parentRef || 'default parent'}</dt>
                    <dd>
                      {parent.controllerName || 'unknown controller'} · accepted {parent.accepted}
                      {parent.acceptedReason ? ` (${parent.acceptedReason})` : ''}
                      {' · '}resolved refs {parent.resolvedRefs}
                      {parent.resolvedReason ? ` (${parent.resolvedReason})` : ''}
                      {parent.acceptedMessage ? ` · ${parent.acceptedMessage}` : ''}
                      {parent.resolvedMessage ? ` · ${parent.resolvedMessage}` : ''}
                    </dd>
                  </div>
                ))}
                {selectedRoute.parents.length === 0 && <div><dt>Status</dt><dd>No parent status reported yet.</dd></div>}
              </dl>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
