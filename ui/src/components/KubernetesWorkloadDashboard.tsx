import { useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import type { KubernetesContext, LampState } from '../types'
import { Inspector } from './Inspector'
import { InventoryList } from './InventoryList'
import type { InventoryColumn } from './InventoryList'
import { KubernetesContextSelect } from './KubernetesContextSelect'
import { StatusLamp } from './StatusLamp'
import { RuntimeGate } from './SectionChrome'

type KubernetesWorkloadDashboardProps<T> = {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
  endpoint: string
  cacheKey: string
  signalTitle: string
  singularLabel: string
  pluralLabel: string
  ariaLabel: string
  filterPlaceholder: string
  emptyMessage: string
  columnsTemplate: string
  columns: InventoryColumn<T>[]
  getKey: (item: T) => string
  getLamp: (item: T) => LampState
  getLampLabel: (item: T) => string
  matchesQuery: (item: T, query: string) => boolean
  getInspectorTitle: (item: T) => string
  getInspectorSubtitle: (item: T) => string
  renderInspector: (item: T) => ReactNode
}

export function KubernetesWorkloadDashboard<T>({
  context,
  contexts,
  onContextChange,
  endpoint,
  cacheKey,
  signalTitle,
  singularLabel,
  pluralLabel,
  ariaLabel,
  filterPlaceholder,
  emptyMessage,
  columnsTemplate,
  columns,
  getKey,
  getLamp,
  getLampLabel,
  matchesQuery,
  getInspectorTitle,
  getInspectorSubtitle,
  renderInspector,
}: KubernetesWorkloadDashboardProps<T>) {
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const status = useKubernetesStatus(context)
  const available = !status.loading && !status.error && (status.data?.available ?? false)
  const resources = usePolledResource<T[]>(
    (signal) => available
      ? apiGet(`/api/kubernetes/${endpoint}?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    6000,
    [context, namespace, available],
    available ? `kubernetes:${context}:${cacheKey}:${namespace}` : undefined,
  )
  const items = resources.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((item) => normalizedQuery === '' || matchesQuery(item, normalizedQuery))
  const selected = items.find((item) => getKey(item) === selectedKey) ?? null
  const loading = status.loading || (available && resources.loading)
  const ready = available && !resources.loading && !resources.error
  const countLabel = items.length === 1 ? singularLabel : pluralLabel

  function selectResource(item: T) {
    returnFocusRef.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    setSelectedKey(getKey(item))
  }

  function closeInspector() {
    setSelectedKey(null)
    window.requestAnimationFrame(() => returnFocusRef.current?.focus())
  }

  return (
    <>
      <section className="fleetRail" aria-label={`Kubernetes ${pluralLabel} status`}>
        <span className="fleetRailTitle">{signalTitle}</span>
        <span className="fleetDatum">
          <StatusLamp state={status.loading ? 'neutral' : available ? 'running' : 'crashed'} />
          {status.loading ? 'Checking' : available ? 'Available' : 'Unavailable'}
        </span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{items.length} {countLabel}</span>
      </section>
      <div className="controlBar kubernetesResourceControlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter {pluralLabel}</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder={filterPlaceholder} onChange={(event) => setQuery(event.target.value)} />
        </label>
        <label className="namespaceField">
          <span>Namespace</span>
          <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
        </label>
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} {pluralLabel}</span>
        <button className="refreshControl" type="button" onClick={() => { status.reload(); resources.reload() }}>Refresh</button>
      </div>
      <div className="workArea">
        {loading ? (
          <article className="empty" role="status"><p>Loading {pluralLabel}...</p></article>
        ) : !ready ? (
          <RuntimeGate
            label={`Kubernetes ${pluralLabel}`}
            enabled={status.data?.enabled ?? false}
            message={resources.error || status.data?.message || status.error}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={getKey}
            columnsTemplate={columnsTemplate}
            getLamp={getLamp}
            getLampLabel={getLampLabel}
            selectedKey={selectedKey}
            onSelect={selectResource}
            ariaLabel={ariaLabel}
            emptyMessage={resources.error || emptyMessage}
            columns={columns}
          />
        )}

        {ready && selected && (
          <Inspector
            title={getInspectorTitle(selected)}
            subtitle={getInspectorSubtitle(selected)}
            onClose={closeInspector}
          >
            {renderInspector(selected)}
          </Inspector>
        )}
      </div>
    </>
  )
}
