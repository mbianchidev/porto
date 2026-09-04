import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useKubernetesStatus } from '../kubernetes'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesCluster,
  KubernetesContext,
  KubernetesMachineSpec,
  RuntimeProviderStatus,
} from '../types'

const DEFAULT_MACHINE: KubernetesMachineSpec = { cpus: 2, memoryMiB: 2048, diskGiB: 20 }
const ClusterTerminal = lazy(() => import('../components/ClusterTerminal'))

type ClusterMutation = 'start' | 'stop' | 'rename' | 'delete' | 'scale' | 'import'

type ClusterMutationControl = {
  disabled: boolean
  begin: () => boolean
  end: () => void
}

function isRunning(cluster: KubernetesCluster) {
  return cluster.state.toLowerCase() === 'running'
}

function isLifecycleLocked(cluster: KubernetesCluster) {
  return ['creating', 'error', 'orphaned'].includes(cluster.state)
}

function clusterElapsed(cluster: KubernetesCluster, now: number) {
  if (cluster.state !== 'creating' || !cluster.stateSince) return ''
  const started = Date.parse(cluster.stateSince)
  if (Number.isNaN(started)) return ''
  return `${Math.max(0, Math.floor((now - started) / 1000))}s`
}

function lifecycleLabel(cluster: KubernetesCluster, mutation?: ClusterMutation) {
  if (mutation === 'stop') return 'Stopping cluster'
  if (mutation === 'start') return 'Starting cluster'
  if (mutation !== undefined) return 'Cluster operation in progress'
  return isRunning(cluster) ? 'Stop cluster' : cluster.state === 'creating' ? 'Cluster is being created' : 'Start cluster'
}

function NodeGroupTab({
  cluster,
  onScaled,
  mutation,
}: {
  cluster: KubernetesCluster
  onScaled: () => void
  mutation: ClusterMutationControl
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [name, setName] = useState('workers')
  const [count, setCount] = useState(1)
  const [machine, setMachine] = useState<KubernetesMachineSpec>(DEFAULT_MACHINE)
  const [version, setVersion] = useState('')
  const [submitting, setSubmitting] = useState(false)

  if (cluster.provider === 'kind') {
    return (
      <section className="drawerPanel">
        <h3>kind node groups</h3>
        <p>kind fixes its node topology at creation time. Recreate this cluster to change its node count.</p>
      </section>
    )
  }

  async function scale(event: FormEvent) {
    event.preventDefault()
    if (name.trim() === '') return
    if (!mutation.begin()) return
    setSubmitting(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${cluster.name}/node-groups/${name.trim()}`, 'POST', {
        version,
        count,
        machine,
        labels: {},
        taints: [],
      })
      notifyNotice('kubernetes', `Scaled node group ${name.trim()} on ${cluster.name} to ${count} node(s).`)
      onScaled()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to scale node group ${name}`))
    } finally {
      setSubmitting(false)
      mutation.end()
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Scale a node group</h3>
      <p className="hintLine">Creates the group if it does not exist yet, or resizes it to the given count.</p>
      <form className="inspectorForm" onSubmit={scale}>
        <label>
          <span>Node group name</span>
          <input type="text" value={name} disabled={mutation.disabled} onChange={(event) => setName(event.target.value)} required />
        </label>
        <label>
          <span>Node count</span>
          <input type="number" min={0} max={32} value={count} disabled={mutation.disabled} onChange={(event) => setCount(Number(event.target.value))} />
        </label>
        <label>
          <span>CPUs per node</span>
          <input type="number" min={1} max={32} value={machine.cpus} disabled={mutation.disabled} onChange={(event) => setMachine({ ...machine, cpus: Number(event.target.value) })} />
        </label>
        <label>
          <span>Memory per node (MiB)</span>
          <input type="number" min={512} step={512} value={machine.memoryMiB} disabled={mutation.disabled} onChange={(event) => setMachine({ ...machine, memoryMiB: Number(event.target.value) })} />
        </label>
        <label>
          <span>Disk per node (GiB)</span>
          <input type="number" min={5} value={machine.diskGiB} disabled={mutation.disabled} onChange={(event) => setMachine({ ...machine, diskGiB: Number(event.target.value) })} />
        </label>
        <label>
          <span>k3s version (optional)</span>
          <input type="text" value={version} placeholder="v1.30.4+k3s1" disabled={mutation.disabled} onChange={(event) => setVersion(event.target.value)} />
        </label>
        <button type="submit" disabled={submitting || mutation.disabled || name.trim() === ''}>{submitting ? 'Scaling…' : 'Scale node group'}</button>
      </form>
    </section>
  )
}

function ImportImageTab({ cluster, mutation }: { cluster: KubernetesCluster; mutation: ClusterMutationControl }) {
  const { notifyError, notifyNotice } = useMessages()
  const [image, setImage] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function importImage(event: FormEvent) {
    event.preventDefault()
    if (image.trim() === '') return
    if (!mutation.begin()) return
    setSubmitting(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${cluster.name}/images/import`, 'POST', { image: image.trim() })
      notifyNotice('kubernetes', `Imported ${image.trim()} into ${cluster.name}.`)
      setImage('')
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to import ${image}`))
    } finally {
      setSubmitting(false)
      mutation.end()
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Import a container image</h3>
      <p className="hintLine">Loads a local or pulled image directly into the cluster's containerd, skipping a registry round trip.</p>
      <form className="inspectorForm" onSubmit={importImage}>
        <label>
          <span>Image reference</span>
          <input type="text" value={image} placeholder="registry/repository:tag" disabled={mutation.disabled} onChange={(event) => setImage(event.target.value)} required />
        </label>
        <button type="submit" disabled={submitting || mutation.disabled || image.trim() === ''}>{submitting ? 'Importing…' : 'Import image'}</button>
      </form>
    </section>
  )
}

export function KubernetesOverview({
  context,
  contexts,
  onContextChange,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
}) {
  const { notifyError, notifyNotice, recordActivity } = useMessages()
  const [selectedClusterName, setSelectedClusterName] = useState<string | null>(null)
  const [clusterTab, setClusterTab] = useState('overview')
  const [creatingCluster, setCreatingCluster] = useState(false)
  const [clusterName, setClusterName] = useState('')
  const [clusterVersion, setClusterVersion] = useState('')
  const [clusterProvider, setClusterProvider] = useState<'kind' | 'k0s' | 'k3s'>('k3s')
  const [controlPlane, setControlPlane] = useState<KubernetesMachineSpec>(DEFAULT_MACHINE)
  const [initialWorkers, setInitialWorkers] = useState(1)
  const [installingProvider, setInstallingProvider] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  const [clusterCreateError, setClusterCreateError] = useState('')
  const [pendingCreations, setPendingCreations] = useState<Record<string, { provider: string; startedAt: number }>>({})
  const [pendingMutations, setPendingMutations] = useState<Record<string, ClusterMutation>>({})
  const mutationsInFlight = useRef(new Set<string>())
  const [clock, setClock] = useState(0)

  const status = useKubernetesStatus(context)
  const clusters = usePolledResource<KubernetesCluster[]>(
    (signal) => apiGet('/api/kubernetes/clusters', signal),
    10000,
    [],
    'kubernetes:clusters',
  )
  const providerTools = usePolledResource<RuntimeProviderStatus[]>(
    (signal) => apiGet('/api/runtime/providers', signal),
    15000,
    [],
    'runtime:providers',
  )
  const items = contexts
  const clusterItems = clusters.data ?? []
  const selectedCluster = clusterItems.find((cluster) => cluster.name === selectedClusterName) ?? null
  const available = status.data?.available ?? false
  const enabled = status.data?.enabled ?? false
  const runningClusters = clusterItems.filter((cluster) => cluster.state === 'running')
  const pendingCreationItems = Object.entries(pendingCreations)
  const hasTimedClusters = pendingCreationItems.length > 0 || clusterItems.some((cluster) => cluster.state === 'creating')

  useEffect(() => {
    if (!hasTimedClusters) return
    const timer = window.setInterval(() => setClock(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [hasTimedClusters])

  function createCluster(event: FormEvent) {
    event.preventDefault()
    const requestedName = clusterName.trim()
    const requestedProvider = clusterProvider
    if (requestedName === '' || Object.hasOwn(pendingCreations, requestedName)) return
    const progress = `Creating ${requestedProvider} cluster ${requestedName}. Provisioning nodes and add-ons can take several minutes.`
    const startedAt = Date.now()
    setClusterCreateError('')
    setClock(startedAt)
    setPendingCreations((current) => ({
      ...current,
      [requestedName]: { provider: requestedProvider, startedAt },
    }))
    recordActivity('info', 'kubernetes', progress)
    const request = {
      name: requestedName,
      provider: requestedProvider,
      version: clusterVersion,
      controlPlane: { ...controlPlane },
      nodeGroups: initialWorkers > 0
        ? [{ name: 'workers', count: initialWorkers, machine: { ...DEFAULT_MACHINE }, labels: {}, taints: [] }]
        : [],
    }
    setClusterName('')
    setClusterVersion('')
    setClusterProvider('k3s')
    setControlPlane(DEFAULT_MACHINE)
    setInitialWorkers(1)
    const creation = apiSend<KubernetesCluster>('/api/kubernetes/clusters', 'POST', request)
    window.setTimeout(clusters.reload, 1000)
    void creation
      .then((cluster) => {
        notifyNotice('kubernetes', `Created ${cluster.provider} cluster ${cluster.name}.`)
        clusters.reload()
        status.reload()
      })
      .catch((err) => {
        const message = errorMessage(err, `Unable to create cluster ${requestedName}`)
        setClusterCreateError(message)
        notifyError('kubernetes', message)
        clusters.reload()
      })
      .finally(() => {
        setPendingCreations((current) => {
          const next = { ...current }
          delete next[requestedName]
          return next
        })
      })
  }

  function beginClusterMutation(clusterName: string, mutation: ClusterMutation) {
    if (mutationsInFlight.current.has(clusterName)) return false
    mutationsInFlight.current.add(clusterName)
    setPendingMutations((current) => ({ ...current, [clusterName]: mutation }))
    return true
  }

  function endClusterMutation(clusterName: string) {
    mutationsInFlight.current.delete(clusterName)
    setPendingMutations((current) => {
      const next = { ...current }
      delete next[clusterName]
      return next
    })
  }

  async function clusterLifecycle(cluster: KubernetesCluster, action: 'start' | 'stop') {
    if (!beginClusterMutation(cluster.name, action)) return
    if (action === 'stop' && cluster.provider === 'kind') {
      notifyNotice('kubernetes', `Stopping ${cluster.name}. KinD workers stop before the control plane, so shutdown can take a while.`)
    }
    try {
      const result = await apiSend<{ status: string; message?: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/${action}`,
        'POST',
      )
      notifyNotice('kubernetes', result.message || `${cluster.name} ${action === 'start' ? 'started' : 'stopped'}.`)
      if (action === 'start') {
        onContextChange(cluster.context)
      } else if (context === cluster.context) {
        const fallback = clusterItems.find((candidate) => candidate.name !== cluster.name && isRunning(candidate))?.context
          || contexts.find((candidate) => candidate.name !== cluster.context)?.name
          || ''
        onContextChange(fallback)
      }
      clusters.reload()
      status.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to ${action} ${cluster.name}`))
    } finally {
      endClusterMutation(cluster.name)
    }
  }

  async function deleteCluster(cluster: KubernetesCluster) {
    if (!window.confirm(`Delete cluster ${cluster.name}? This permanently removes its control-plane and worker nodes.`)) return
    if (!beginClusterMutation(cluster.name, 'delete')) return
    try {
      await apiSend(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}?confirm=true`, 'DELETE')
      notifyNotice('kubernetes', `Deleted cluster ${cluster.name}.`)
      if (selectedClusterName === cluster.name) setSelectedClusterName(null)
      clusters.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to delete ${cluster.name}`))
    } finally {
      endClusterMutation(cluster.name)
    }
  }

  async function renameCluster(event: FormEvent) {
    event.preventDefault()
    if (!selectedCluster || renameDraft.trim() === '' || renameDraft.trim() === selectedCluster.name) return
    const previousName = selectedCluster.name
    if (!beginClusterMutation(previousName, 'rename')) return
    try {
      const previousContext = selectedCluster.context
      const result = await apiSend<{ name: string; context: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(previousName)}/rename`,
        'POST',
        { name: renameDraft.trim() },
      )
      notifyNotice('kubernetes', `Renamed cluster ${previousName} to ${result.name}.`)
      setSelectedClusterName(result.name)
      setRenameDraft(result.name)
      if (context === previousContext) onContextChange(result.context)
      clusters.reload()
      status.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to rename ${selectedCluster.name}`))
    } finally {
      endClusterMutation(previousName)
    }
  }

  async function installProvider(name: RuntimeProviderStatus['name']) {
    setInstallingProvider(name)
    try {
      await apiSend(`/api/runtime/providers/${name}/install`, 'POST')
      notifyNotice('kubernetes', `${name} provider installed.`)
      providerTools.reload()
      status.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to install ${name}`))
    } finally {
      setInstallingProvider(null)
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Cluster signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum">
          <StatusLamp state={runningClusters.length > 0 ? 'running' : 'stopped'} />
          <small>Managed clusters</small>
          <strong>{runningClusters.length} running</strong>
        </span>
        <span className="fleetDatum"><small>Context</small><strong>{status.data?.context || context || 'default'}</strong></span>
        {status.data?.serverVersion && <span className="fleetDatum"><small>Server</small><strong>{status.data.serverVersion}</strong></span>}
        <span className="fleetMessage">{items.length} configured context(s) · {clusterItems.length} Porto cluster(s)</span>
      </section>
      <div className="controlBar">
        <span className="filterResultCount" aria-live="polite">Active context: {context || 'cluster default'}</span>
        <button className="refreshControl" type="button" onClick={() => { status.reload(); clusters.reload() }}>Refresh</button>
        <button type="button" disabled={!enabled} onClick={() => {
          setSelectedClusterName(null)
          setCreatingCluster(true)
        }}>New cluster{pendingCreationItems.length > 0 ? ` (${pendingCreationItems.length} active)` : ''}</button>
      </div>
      <div className="workArea">
        {!enabled ? (
          <RuntimeGate label="Kubernetes" enabled={false} message={status.data?.message} />
        ) : !available ? (
          <RuntimeGate label="Kubernetes" enabled message={status.data?.message || status.error || 'Porto could not reach kubectl or a cluster.'} />
        ) : (
          <InventoryList
            items={items}
            getKey={(item) => item.name}
            columnsTemplate="minmax(160px,1.2fr) minmax(160px,1fr) minmax(140px,1fr) minmax(110px,0.6fr)"
            selectedKey={context || null}
            onSelect={(item) => onContextChange(item.name)}
            ariaLabel="Kubernetes contexts"
            emptyMessage="No kubeconfig contexts found."
            columns={[
              { header: 'Context', render: (item) => <strong>{item.name}</strong> },
              { header: 'Cluster', className: 'mono', render: (item) => item.cluster },
              { header: 'Namespace', className: 'mono', render: (item) => item.namespace || 'default' },
              { header: 'Current', className: 'mono', render: (item) => (item.current ? 'kubeconfig default' : '—') },
            ]}
          />
        )}
      </div>

      {enabled && (
        <>
          <h2 className="sectionSubhead">Managed clusters</h2>
          <div className="workArea">
            <InventoryList
              items={clusterItems}
              getKey={(cluster) => cluster.name}
              columnsTemplate="12px minmax(130px,1fr) minmax(80px,0.5fr) minmax(90px,0.6fr) minmax(70px,0.4fr) minmax(140px,1fr) minmax(140px,1fr) minmax(55px,0.35fr)"
              selectedKey={selectedClusterName}
              onSelect={(cluster) => {
                setCreatingCluster(false)
                setSelectedClusterName(cluster.name)
                setRenameDraft(cluster.name)
                setClusterTab('overview')
              }}
              getLamp={(cluster) => lampStateFor(cluster.state)}
              getLampLabel={(cluster) => cluster.state}
              ariaLabel="Porto-provisioned Kubernetes clusters"
              emptyMessage={clusters.error || 'No Porto-provisioned clusters yet. Create one to get started.'}
              columns={[
                { header: 'Name', render: (cluster) => <strong>{cluster.name}</strong> },
                { header: 'Provider', className: 'mono', render: (cluster) => cluster.provider },
                { header: 'State', className: 'mono', render: (cluster) => cluster.state },
                { header: 'Elapsed', className: 'mono', render: (cluster) => clusterElapsed(cluster, clock) || '—' },
                { header: 'Context', className: 'mono', render: (cluster) => cluster.context },
                { header: 'Server', className: 'mono', render: (cluster) => cluster.server || '—' },
                { header: 'Nodes', className: 'mono', render: (cluster) => cluster.nodes?.length ?? 0 },
              ]}
              renderActions={(cluster) => {
                const pendingMutation = pendingMutations[cluster.name]
                return isRunning(cluster) || pendingMutation === 'stop'
                  ? <ActionButton
                      label={lifecycleLabel(cluster, pendingMutation)}
                      icon="stop"
                      disabled={pendingMutation !== undefined}
                      onClick={() => clusterLifecycle(cluster, 'stop')}
                    />
                  : <ActionButton
                      label={lifecycleLabel(cluster, pendingMutation)}
                      icon="play"
                      disabled={isLifecycleLocked(cluster) || pendingMutation !== undefined}
                      onClick={() => clusterLifecycle(cluster, 'start')}
                    />
              }}
            />

            {selectedCluster && !creatingCluster && (
              <Inspector title={selectedCluster.name} subtitle={selectedCluster.context} onClose={() => setSelectedClusterName(null)}>
                <InspectorTabs
                  tabs={isLifecycleLocked(selectedCluster)
                    ? [{ id: 'overview', label: 'Overview' }]
                    : [
                        { id: 'overview', label: 'Overview' },
                        { id: 'terminal', label: 'k9s terminal' },
                        { id: 'nodeGroup', label: 'Node group' },
                        { id: 'importImage', label: 'Import image' },
                      ]}
                  activeID={clusterTab}
                  onSelect={setClusterTab}
                />
                {clusterTab === 'overview' && (
                  <section className="drawerPanel">
                    <h3>Cluster detail</h3>
                    {selectedCluster.state === 'orphaned' && (
                      <p className="errorLine">The kubeconfig exists but Porto ownership metadata is missing. Delete and recreate the cluster to restore full lifecycle management.</p>
                    )}
                    {selectedCluster.state === 'creating' && (
                      <p className="hintLine" role="status">Porto is still creating nodes and configuring cluster add-ons. You can close this inspector; creation continues in the daemon.</p>
                    )}
                    {selectedCluster.state === 'error' && (
                      <p className="errorLine" role="alert">{selectedCluster.message || 'Cluster creation failed. Delete this failed cluster record before retrying the same name.'}</p>
                    )}
                    {selectedCluster.state === 'broken' && (
                      <p className="errorLine">The control-plane node is missing. Starting a KinD cluster will recreate it from the saved cluster configuration.</p>
                    )}
                    {pendingMutations[selectedCluster.name] === 'stop' && selectedCluster.provider === 'kind' && (
                      <p className="hintLine" role="status">Stopping KinD worker containers before the control plane. This can take a while.</p>
                    )}
                    <dl className="runtimeGrid">
                      <div><dt>Context</dt><dd>{selectedCluster.context}</dd></div>
                      <div><dt>Provider</dt><dd>{selectedCluster.provider}</dd></div>
                      <div><dt>State</dt><dd>{selectedCluster.state}</dd></div>
                      {clusterElapsed(selectedCluster, clock) && <div><dt>Elapsed</dt><dd>{clusterElapsed(selectedCluster, clock)}</dd></div>}
                      <div><dt>Server</dt><dd>{selectedCluster.server || '—'}</dd></div>
                      <div><dt>Kubeconfig</dt><dd>{selectedCluster.kubeconfigPath}</dd></div>
                      <div><dt>Nodes</dt><dd>{selectedCluster.nodes?.join(', ') || '—'}</dd></div>
                    </dl>
                    <form className="inspectorForm inline" onSubmit={renameCluster}>
                      <label>
                        <span>Cluster name</span>
                        <input
                          type="text"
                          value={renameDraft}
                          disabled={isLifecycleLocked(selectedCluster) || pendingMutations[selectedCluster.name] !== undefined}
                          onChange={(event) => setRenameDraft(event.target.value)}
                          required
                        />
                      </label>
                      <button
                        type="submit"
                        disabled={isLifecycleLocked(selectedCluster) || pendingMutations[selectedCluster.name] !== undefined || renameDraft.trim() === '' || renameDraft.trim() === selectedCluster.name}
                      >
                        {pendingMutations[selectedCluster.name] === 'rename' ? 'Renaming…' : 'Rename cluster'}
                      </button>
                    </form>
                    <div className="maintenanceBar">
                      <span>Maintenance controls</span>
                      <div className="actions">
                        {isRunning(selectedCluster) || pendingMutations[selectedCluster.name] === 'stop'
                          ? <ActionButton
                              label={lifecycleLabel(selectedCluster, pendingMutations[selectedCluster.name])}
                              icon="stop"
                              disabled={pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'stop')}
                            />
                          : <ActionButton
                              label={lifecycleLabel(selectedCluster, pendingMutations[selectedCluster.name])}
                              icon="play"
                              disabled={isLifecycleLocked(selectedCluster) || pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'start')}
                            />}
                        <ActionButton
                          className="removeButton"
                          label={pendingMutations[selectedCluster.name] === 'delete' ? 'Deleting cluster' : 'Delete cluster'}
                          icon="remove"
                          disabled={pendingMutations[selectedCluster.name] !== undefined}
                          onClick={() => deleteCluster(selectedCluster)}
                        />
                      </div>
                    </div>
                  </section>
                )}
                {clusterTab === 'terminal' && (
                  <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading k9s terminal…</div></section>}>
                    <ClusterTerminal key={`${selectedCluster.name}:${selectedCluster.state}`} cluster={selectedCluster} />
                  </Suspense>
                )}
                {clusterTab === 'nodeGroup' && (
                  <NodeGroupTab
                    cluster={selectedCluster}
                    onScaled={clusters.reload}
                    mutation={{
                      disabled: pendingMutations[selectedCluster.name] !== undefined,
                      begin: () => beginClusterMutation(selectedCluster.name, 'scale'),
                      end: () => endClusterMutation(selectedCluster.name),
                    }}
                  />
                )}
                {clusterTab === 'importImage' && (
                  <ImportImageTab
                    cluster={selectedCluster}
                    mutation={{
                      disabled: pendingMutations[selectedCluster.name] !== undefined,
                      begin: () => beginClusterMutation(selectedCluster.name, 'import'),
                      end: () => endClusterMutation(selectedCluster.name),
                    }}
                  />
                )}
              </Inspector>
            )}

            {creatingCluster && (
              <Inspector title="New cluster" subtitle="Managed by Porto" onClose={() => setCreatingCluster(false)}>
                <form className="inspectorForm" onSubmit={createCluster}>
                  {pendingCreationItems.length > 0 && (
                    <div className="debugToolboxDetails" role="status" aria-live="polite">
                      {pendingCreationItems.map(([name, creation]) => (
                        <p key={name}><strong>{name}</strong> · {creation.provider} · {Math.max(0, Math.floor((clock - creation.startedAt) / 1000))}s</p>
                      ))}
                    </div>
                  )}
                  {clusterCreateError && <p className="errorLine" role="alert">{clusterCreateError}</p>}
                  <section className="providerReadiness" aria-label="Kubernetes provider readiness">
                    {(providerTools.data ?? []).filter((provider) => provider.name !== 'qemu').map((provider) => (
                      <div className={`integrationStatus ${provider.installed ? 'ready' : 'missing'}`} key={provider.name}>
                        <strong>{provider.name}</strong>
                        <span>{provider.installed ? provider.version : provider.message}</span>
                        {!provider.installed && (
                          <button type="button" disabled={installingProvider !== null} onClick={() => installProvider(provider.name)}>
                            {installingProvider === provider.name ? 'Installing…' : `Install ${provider.name}`}
                          </button>
                        )}
                      </div>
                    ))}
                  </section>
                  <label>
                    <span>Name</span>
                    <input type="text" value={clusterName} placeholder="dev-cluster" onChange={(event) => setClusterName(event.target.value)} required />
                  </label>
                  <label>
                    <span>Provider</span>
                    <select value={clusterProvider} onChange={(event) => setClusterProvider(event.target.value as 'kind' | 'k0s' | 'k3s')}>
                      <option value="kind">kind — Kubernetes in Porto containers</option>
                      <option value="k3s">k3s — lightweight Kubernetes on Lima VMs</option>
                      <option value="k0s">k0s — conformant Kubernetes on Lima VMs</option>
                    </select>
                  </label>
                  <label>
                    <span>Version (optional)</span>
                    <input
                      type="text"
                      value={clusterVersion}
                      placeholder={clusterProvider === 'kind' ? 'v1.37.0' : clusterProvider === 'k3s' ? 'v1.36.0+k3s1' : 'v1.36.0+k0s.0'}
                      onChange={(event) => setClusterVersion(event.target.value)}
                    />
                  </label>
                  <label>
                    <span>Control-plane CPUs</span>
                    <input type="number" min={1} max={32} value={controlPlane.cpus} onChange={(event) => setControlPlane({ ...controlPlane, cpus: Number(event.target.value) })} />
                  </label>
                  <label>
                    <span>Control-plane memory (MiB)</span>
                    <input type="number" min={512} step={512} value={controlPlane.memoryMiB} onChange={(event) => setControlPlane({ ...controlPlane, memoryMiB: Number(event.target.value) })} />
                  </label>
                  <label>
                    <span>Control-plane disk (GiB)</span>
                    <input type="number" min={5} value={controlPlane.diskGiB} onChange={(event) => setControlPlane({ ...controlPlane, diskGiB: Number(event.target.value) })} />
                  </label>
                  <label>
                    <span>Initial worker nodes</span>
                    <input type="number" min={0} max={16} value={initialWorkers} onChange={(event) => setInitialWorkers(Number(event.target.value))} />
                  </label>
                  <p className="hintLine">
                    {clusterProvider === 'kind'
                      ? 'Creates Kubernetes nodes as privileged containers through the Porto Docker endpoint.'
                      : `Creates a ${clusterProvider} control plane on a Porto-managed Lima VM. Add worker node groups afterward.`}
                  </p>
                  <button type="submit" disabled={clusterName.trim() === '' || Object.hasOwn(pendingCreations, clusterName.trim())}>Create cluster</button>
                </form>
              </Inspector>
            )}
          </div>
        </>
      )}
    </>
  )
}
