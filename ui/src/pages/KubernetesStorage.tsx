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
  KubernetesPersistentVolume,
  KubernetesPersistentVolumeClaim,
} from '../types'

type StorageTab = 'volumes' | 'claims'

function storageLamp(phase: string): 'running' | 'starting' | 'crashed' | 'neutral' {
  switch (phase.toLocaleLowerCase()) {
    case 'bound':
    case 'available':
      return 'running'
    case 'pending':
    case 'released':
      return 'starting'
    case 'failed':
      return 'crashed'
    default:
      return 'neutral'
  }
}

export function KubernetesStorage({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const [tab, setTab] = useState<StorageTab>('volumes')
  const [namespace, setNamespace] = useState('')
  const [query, setQuery] = useState('')
  const [selectedKey, setSelectedKey] = useState<string | null>(null)
  const returnFocusRef = useRef<HTMLElement | null>(null)
  const status = useKubernetesStatus(context)
  const available = status.data?.available ?? false
  const volumes = usePolledResource<KubernetesPersistentVolume[]>(
    (signal) => available && tab === 'volumes'
      ? apiGet(`/api/kubernetes/persistent-volumes?context=${encodeURIComponent(context)}`, signal)
      : Promise.resolve([]),
    8000,
    [context, available, tab],
    available && tab === 'volumes' ? `kubernetes:${context}:persistent-volumes` : undefined,
  )
  const claims = usePolledResource<KubernetesPersistentVolumeClaim[]>(
    (signal) => available && tab === 'claims'
      ? apiGet(`/api/kubernetes/persistent-volume-claims?context=${encodeURIComponent(context)}&namespace=${encodeURIComponent(namespace)}`, signal)
      : Promise.resolve([]),
    8000,
    [context, namespace, available, tab],
    available && tab === 'claims' ? `kubernetes:${context}:persistent-volume-claims:${namespace}` : undefined,
  )
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const volumeItems = (volumes.data ?? []).map((volume) => ({
    ...volume,
    accessModes: volume.accessModes ?? [],
  }))
  const claimItems = (claims.data ?? []).map((claim) => ({
    ...claim,
    accessModes: claim.accessModes ?? [],
  }))
  const filteredVolumes = volumeItems.filter((volume) => normalizedQuery === '' || [
    volume.name,
    volume.phase,
    volume.storageClass,
    volume.claim,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const filteredClaims = claimItems.filter((claim) => normalizedQuery === '' || [
    claim.name,
    claim.namespace,
    claim.phase,
    claim.storageClass,
    claim.volume,
  ].some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const selectedVolume = tab === 'volumes'
    ? volumeItems.find((volume) => volume.name === selectedKey) ?? null
    : null
  const claimKey = (claim: KubernetesPersistentVolumeClaim) => `${claim.namespace}/${claim.name}`
  const selectedClaim = tab === 'claims'
    ? claimItems.find((claim) => claimKey(claim) === selectedKey) ?? null
    : null
  const activeCount = tab === 'volumes' ? volumeItems.length : claimItems.length
  const filteredCount = tab === 'volumes' ? filteredVolumes.length : filteredClaims.length
  const activeError = tab === 'volumes' ? volumes.error : claims.error

  function selectTab(id: string) {
    setTab(id as StorageTab)
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
      <section className="fleetRail" aria-label="Kubernetes storage status">
        <span className="fleetRailTitle">Storage signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{context || 'default'}</strong></span>
        <span className="fleetMessage">{activeCount} {tab === 'volumes' ? 'persistent volume(s)' : 'persistent volume claim(s)'}</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter Kubernetes storage</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter by name, phase, class, or binding" onChange={(event) => setQuery(event.target.value)} />
        </label>
        {tab === 'claims' && (
          <label className="namespaceField">
            <span>Namespace</span>
            <input type="text" value={namespace} placeholder="all namespaces" onChange={(event) => setNamespace(event.target.value)} />
          </label>
        )}
        <KubernetesContextSelect contexts={contexts} value={context} onChange={onContextChange} />
        <span className="filterResultCount" aria-live="polite">{filteredCount} / {activeCount}</span>
        <button className="refreshControl" type="button" onClick={() => {
          status.reload()
          if (tab === 'volumes') volumes.reload()
          else claims.reload()
        }}>Refresh</button>
      </div>
      <InspectorTabs
        tabs={[
          { id: 'volumes', label: 'Persistent volumes' },
          { id: 'claims', label: 'Persistent volume claims' },
        ]}
        activeID={tab}
        onSelect={selectTab}
      />
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Kubernetes storage" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : tab === 'volumes' ? (
          <InventoryList
            items={filteredVolumes}
            getKey={(volume) => volume.name}
            columnsTemplate="12px minmax(150px,1fr) minmax(90px,0.5fr) minmax(90px,0.5fr) minmax(130px,0.8fr) minmax(150px,1fr) minmax(70px,0.4fr)"
            getLamp={(volume) => storageLamp(volume.phase)}
            getLampLabel={(volume) => volume.phase}
            selectedKey={selectedKey}
            onSelect={(volume) => selectResource(volume.name)}
            ariaLabel="Kubernetes persistent volumes"
            emptyMessage={activeError || 'No persistent volumes found.'}
            columns={[
              { header: 'Name', render: (volume) => <strong>{volume.name}</strong> },
              { header: 'Phase', className: 'mono', render: (volume) => volume.phase },
              { header: 'Capacity', className: 'mono', render: (volume) => volume.capacity || '—' },
              { header: 'Storage class', className: 'mono', render: (volume) => volume.storageClass || '—' },
              { header: 'Claim', className: 'mono', render: (volume) => volume.claim || '—' },
              { header: 'Age', className: 'mono', render: (volume) => volume.age },
            ]}
          />
        ) : (
          <InventoryList
            items={filteredClaims}
            getKey={claimKey}
            columnsTemplate="12px minmax(140px,1fr) minmax(110px,0.7fr) minmax(80px,0.45fr) minmax(90px,0.5fr) minmax(130px,0.8fr) minmax(140px,0.9fr) minmax(70px,0.4fr)"
            getLamp={(claim) => storageLamp(claim.phase)}
            getLampLabel={(claim) => claim.phase}
            selectedKey={selectedKey}
            onSelect={(claim) => selectResource(claimKey(claim))}
            ariaLabel="Kubernetes persistent volume claims"
            emptyMessage={activeError || 'No persistent volume claims found.'}
            columns={[
              { header: 'Name', render: (claim) => <strong>{claim.name}</strong> },
              { header: 'Namespace', className: 'mono', render: (claim) => claim.namespace },
              { header: 'Phase', className: 'mono', render: (claim) => claim.phase },
              { header: 'Requested', className: 'mono', render: (claim) => claim.requested || '—' },
              { header: 'Storage class', className: 'mono', render: (claim) => claim.storageClass || '—' },
              { header: 'Volume', className: 'mono', render: (claim) => claim.volume || '—' },
              { header: 'Age', className: 'mono', render: (claim) => claim.age },
            ]}
          />
        )}

        {selectedVolume && (
          <Inspector title={selectedVolume.name} subtitle={`${selectedVolume.phase} · PersistentVolume`} onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>Persistent volume detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Capacity</dt><dd>{selectedVolume.capacity || '—'}</dd></div>
                <div><dt>Storage class</dt><dd>{selectedVolume.storageClass || '—'}</dd></div>
                <div><dt>Claim</dt><dd>{selectedVolume.claim || 'unbound'}</dd></div>
                <div><dt>Access modes</dt><dd>{selectedVolume.accessModes.join(', ') || '—'}</dd></div>
                <div><dt>Reclaim policy</dt><dd>{selectedVolume.reclaimPolicy || '—'}</dd></div>
                <div><dt>Volume mode</dt><dd>{selectedVolume.volumeMode || '—'}</dd></div>
                <div><dt>Age</dt><dd>{selectedVolume.age}</dd></div>
              </dl>
            </section>
          </Inspector>
        )}
        {selectedClaim && (
          <Inspector title={selectedClaim.name} subtitle={`${selectedClaim.namespace} · PersistentVolumeClaim`} onClose={closeInspector}>
            <section className="drawerPanel">
              <h3>Persistent volume claim detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Phase</dt><dd>{selectedClaim.phase}</dd></div>
                <div><dt>Requested</dt><dd>{selectedClaim.requested || '—'}</dd></div>
                <div><dt>Capacity</dt><dd>{selectedClaim.capacity || 'pending'}</dd></div>
                <div><dt>Storage class</dt><dd>{selectedClaim.storageClass || '—'}</dd></div>
                <div><dt>Bound volume</dt><dd>{selectedClaim.volume || 'unbound'}</dd></div>
                <div><dt>Access modes</dt><dd>{selectedClaim.accessModes.join(', ') || '—'}</dd></div>
                <div><dt>Volume mode</dt><dd>{selectedClaim.volumeMode || '—'}</dd></div>
                <div><dt>Age</dt><dd>{selectedClaim.age}</dd></div>
              </dl>
            </section>
          </Inspector>
        )}
      </div>
    </>
  )
}
