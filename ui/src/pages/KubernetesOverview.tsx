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
import { ClusterUpgrade } from './ClusterUpgrade'
import type {
  KubernetesCluster,
  KubernetesClusterProvider,
  KubernetesContext,
  KubernetesMachineSpec,
  RuntimeProviderStatus,
  EtcqliteInstance,
  EtcqliteStatus,
} from '../types'

const DEFAULT_MACHINE: KubernetesMachineSpec = { cpus: 2, memoryMiB: 2048, diskGiB: 20 }
const ClusterTerminal = lazy(() => import('../components/ClusterTerminal'))
const ClusterAutoscaling = lazy(() => import('./ClusterAutoscaling'))

type ClusterMutation = 'start' | 'stop' | 'restart' | 'rename' | 'delete' | 'scale' | 'import' | 'upgrade' | 'autoscaling'

type ClusterMutationControl = {
  disabled: boolean
  begin: () => boolean
  end: () => void
}

function isRunning(cluster: KubernetesCluster) {
  return cluster.state.toLowerCase() === 'running'
}

function guestReachableDatastore(instance: EtcqliteInstance) {
  return ['ready', 'degraded'].includes(instance.state) && instance.members.length > 0 && instance.members.every((member) => {
    if (!URL.canParse(member.clientURL)) return false
    const address = new URL(member.clientURL)
    const host = address.hostname.toLowerCase()
    return address.protocol === 'https:' && !host.startsWith('127.') && host !== '[::1]' && host !== 'localhost' &&
      !host.endsWith('.localhost') && !member.listen.startsWith('127.') && !member.listen.startsWith('[::1]')
  })
}

function isLifecycleLocked(cluster: KubernetesCluster) {
  return ['creating', 'error', 'orphaned', 'upgrading', 'upgrade-failed', 'starting', 'stopping', 'deleting'].includes(cluster.state)
}

function supportsLifecycle(cluster: KubernetesCluster, action: 'start' | 'stop' | 'restart') {
  return cluster.capabilities?.[action] ?? cluster.provider !== 'apple'
}

function clusterElapsed(cluster: KubernetesCluster, now: number) {
  if (!['creating', 'starting', 'stopping'].includes(cluster.state) || !cluster.stateSince) return ''
  const started = Date.parse(cluster.stateSince)
  if (Number.isNaN(started)) return ''
  return `${Math.max(0, Math.floor((now - started) / 1000))}s`
}

function lifecycleLabel(cluster: KubernetesCluster, mutation?: ClusterMutation) {
  if (mutation === 'stop') return 'Stopping cluster'
  if (mutation === 'start') return 'Starting cluster'
  if (mutation === 'restart') return 'Restarting cluster'
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

  if (cluster.provider === 'kind' || cluster.provider === 'apple') {
    return (
      <section className="drawerPanel">
        <h3>{cluster.provider === 'apple' ? 'Apple Cluster' : 'kind'} node groups</h3>
        <p>This provider fixes node topology at creation time. Changing the node count requires explicit deletion and recreation, which removes cluster data.</p>
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
  const locked = mutation.disabled || (cluster.provider === 'apple' && !(cluster.capabilities?.imageLoading ?? isRunning(cluster)))

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
      <p className="hintLine">{cluster.provider === 'apple'
        ? 'Loads an arm64 image from the Apple container image store into every running cluster node. Build or pull it with container first; Porto does not share the entire host image store.'
        : "Loads a local or pulled image directly into the cluster's containerd, skipping a registry round trip."}</p>
      <form className="inspectorForm" onSubmit={importImage}>
        <label>
          <span>Image reference</span>
          <input type="text" value={image} placeholder="registry/repository:tag" disabled={locked} onChange={(event) => setImage(event.target.value)} required />
        </label>
        <button type="submit" disabled={submitting || locked || image.trim() === ''}>{submitting ? 'Importing…' : 'Import image'}</button>
      </form>
    </section>
  )
}

export function KubernetesOverview({
  context,
  contexts,
  onContextChange,
  onClusterRenamed,
}: {
  context: string
  contexts: KubernetesContext[]
  onContextChange: (context: string) => void
  onClusterRenamed: (previousName: string, nextName: string, previousContext: string, nextContext: string) => void
}) {
  const { notifyError, notifyNotice, recordActivity } = useMessages()
  const [selectedClusterName, setSelectedClusterName] = useState<string | null>(null)
  const [clusterTab, setClusterTab] = useState('overview')
  const [creatingCluster, setCreatingCluster] = useState(false)
  const [clusterName, setClusterName] = useState('')
  const [clusterVersion, setClusterVersion] = useState('')
  const [clusterProvider, setClusterProvider] = useState<KubernetesClusterProvider>('k3s')
  const [clusterDatastore, setClusterDatastore] = useState('')
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
  const datastores = usePolledResource<EtcqliteStatus | null>(
    (signal) => creatingCluster && clusterProvider === 'k3s' ? apiGet('/api/integrations/etcqlite', signal) : Promise.resolve(null),
    10000, [creatingCluster, clusterProvider],
  )
  const items = contexts
  const clusterItems = clusters.data ?? []
  const selectedCluster = clusterItems.find((cluster) => cluster.name === selectedClusterName) ?? null
  const available = status.data?.available ?? false
  const enabled = status.data?.enabled ?? false
  const runningClusters = clusterItems.filter((cluster) => cluster.state === 'running')
  const pendingCreationItems = Object.entries(pendingCreations)
  const hasTimedClusters = pendingCreationItems.length > 0 || clusterItems.some((cluster) => ['creating', 'starting', 'stopping'].includes(cluster.state))

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
    const datastore = datastores.data?.instances?.find((instance) => instance.name === clusterDatastore)
    if (requestedProvider === 'k3s' && clusterDatastore !== '' && (!datastore || !guestReachableDatastore(datastore) || datastores.error)) {
      notifyError('kubernetes', 'Selected Etcqlite datastore is not ready or guest-reachable. No cluster was created.')
      return
    }
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
      controlPlane: { ...controlPlane, ...(requestedProvider === 'apple' ? { diskGiB: 0 } : {}) },
      nodeGroups: initialWorkers > 0
        ? [{ name: 'workers', count: initialWorkers, machine: { ...DEFAULT_MACHINE, ...(requestedProvider === 'apple' ? { diskGiB: 0 } : {}) }, labels: {}, taints: [] }]
        : [],
      ...(requestedProvider === 'k3s' && datastore ? { etcqlite: { name: datastore.name, clusterID: datastore.clusterID } } : {}),
    }
    setClusterName('')
    setClusterVersion('')
    setClusterProvider('k3s')
    setClusterDatastore('')
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

  async function clusterLifecycle(cluster: KubernetesCluster, action: 'start' | 'stop' | 'restart') {
    if (!beginClusterMutation(cluster.name, action)) return
    if (action === 'stop' && cluster.provider === 'kind') {
      notifyNotice('kubernetes', `Stopping ${cluster.name}. KinD workers stop before the control plane, so shutdown can take a while.`)
    }
    try {
      const result = await apiSend<{ status: string; message?: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/${action}`,
        'POST',
      )
      notifyNotice('kubernetes', result.message || `${cluster.name} ${action === 'start' ? 'started' : action === 'restart' ? 'restarted' : 'stopped'}.`)
      if (action !== 'stop') {
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
      clusters.reload()
      status.reload()
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
      const result = await apiSend<{ name: string; context: string; message?: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(previousName)}/rename`,
        'POST',
        { name: renameDraft.trim() },
      )
      notifyNotice('kubernetes', result.message || `Renamed cluster ${previousName} to ${result.name}.`)
      clusters.update((current) => current?.map((cluster) => (
        cluster.name === previousName
          ? { ...cluster, name: result.name, context: result.context }
          : cluster
      )) ?? current)
      onClusterRenamed(previousName, result.name, previousContext, result.context)
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
                      disabled={!supportsLifecycle(cluster, 'stop') || isLifecycleLocked(cluster) || pendingMutation !== undefined}
                      onClick={() => clusterLifecycle(cluster, 'stop')}
                    />
                  : <ActionButton
                      label={lifecycleLabel(cluster, pendingMutation)}
                      icon="play"
                      disabled={!supportsLifecycle(cluster, 'start') || isLifecycleLocked(cluster) || pendingMutation !== undefined}
                      onClick={() => clusterLifecycle(cluster, 'start')}
                    />
              }}
            />

            {selectedCluster && !creatingCluster && (
              <Inspector title={selectedCluster.name} subtitle={selectedCluster.context} onClose={() => setSelectedClusterName(null)}>
                <InspectorTabs
                  tabs={selectedCluster.state === 'upgrade-failed'
                    ? [{ id: 'overview', label: 'Overview' }, { id: 'upgrade', label: 'Upgrade' }]
                    : isLifecycleLocked(selectedCluster)
                    ? [{ id: 'overview', label: 'Overview' }]
                    : [
                        { id: 'overview', label: 'Overview' },
                        { id: 'terminal', label: 'k9s terminal' },
                        { id: 'nodeGroup', label: 'Node group' },
                        { id: 'importImage', label: 'Import image' },
                        { id: 'upgrade', label: 'Upgrade' },
                        { id: 'autoscaling', label: 'Autoscaling' },
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
                    {selectedCluster.provider === 'apple' && (
                      <p className="hintLine">{selectedCluster.capabilities?.nativeNodes
                        ? 'Porto owns these native node VMs directly. Stop gracefully shuts down workers before the control plane; Start verifies Kubernetes and workload recovery without rebuilding data. Node scaling and version upgrades remain unavailable.'
                        : 'This existing cluster uses Apple’s protected cluster wrapper. Porto will not silently migrate or rebuild its nodes; wrapper-managed start/stop remains unavailable.'}</p>
                    )}
                    {selectedCluster.lifecycle && !selectedCluster.lifecycle.completedAt && (
                      <div className="debugToolboxDetails" role="status" aria-live="polite">
                        <p>{selectedCluster.lifecycle.goal}: {selectedCluster.lifecycle.stage}</p>
                        {Object.entries(selectedCluster.lifecycle.nodes).map(([name, progress]) => <p key={name}><strong>{name}</strong>: {progress}</p>)}
                      </div>
                    )}
                    {selectedCluster.provider === 'apple' && selectedCluster.state !== 'error' && selectedCluster.message && (
                      <p className="errorLine" role="alert">{selectedCluster.message}</p>
                    )}
                    {selectedCluster.state === 'broken' && (
                      <p className="errorLine">The control-plane node is missing. Starting a KinD cluster will recreate it from the saved cluster configuration.</p>
                    )}
                    {selectedCluster.state === 'upgrading' && <p className="hintLine" role="status">Upgrading Kubernetes. Cluster mutations are locked until all nodes and applications are ready.</p>}
                    {selectedCluster.state === 'upgrade-failed' && <p className="errorLine" role="alert">{selectedCluster.message} Open Upgrade to retry or recover.</p>}
                    {pendingMutations[selectedCluster.name] === 'stop' && selectedCluster.provider === 'kind' && (
                      <p className="hintLine" role="status">Stopping KinD worker containers before the control plane. This can take a while.</p>
                    )}
                    <dl className="runtimeGrid">
                      <div><dt>Context</dt><dd>{selectedCluster.context}</dd></div>
                      <div><dt>Provider</dt><dd>{selectedCluster.provider}</dd></div>
                      <div><dt>Datastore</dt><dd>{selectedCluster.etcqlite ? `Etcqlite ${selectedCluster.etcqlite.name} (${selectedCluster.etcqlite.clusterID})` : 'Provider default'}</dd></div>
                      <div><dt>Version</dt><dd>{selectedCluster.version || 'Unpinned'}</dd></div>
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
                        {selectedCluster.capabilities?.nativeNodes ? (
                          <>
                            <ActionButton
                              label="Start cluster"
                              icon="play"
                              disabled={!supportsLifecycle(selectedCluster, 'start') || isLifecycleLocked(selectedCluster) || isRunning(selectedCluster) || pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'start')}
                            />
                            <ActionButton
                              label="Stop cluster"
                              icon="stop"
                              disabled={!supportsLifecycle(selectedCluster, 'stop') || isLifecycleLocked(selectedCluster) || selectedCluster.state === 'stopped' || pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'stop')}
                            />
                          </>
                        ) : isRunning(selectedCluster) || pendingMutations[selectedCluster.name] === 'stop'
                          ? <ActionButton
                              label={lifecycleLabel(selectedCluster, pendingMutations[selectedCluster.name])}
                              icon="stop"
                              disabled={!supportsLifecycle(selectedCluster, 'stop') || pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'stop')}
                            />
                          : <ActionButton
                              label={lifecycleLabel(selectedCluster, pendingMutations[selectedCluster.name])}
                              icon="play"
                              disabled={!supportsLifecycle(selectedCluster, 'start') || isLifecycleLocked(selectedCluster) || pendingMutations[selectedCluster.name] !== undefined}
                              onClick={() => clusterLifecycle(selectedCluster, 'start')}
                            />}
                        {supportsLifecycle(selectedCluster, 'restart') && (
                          <ActionButton
                            label="Restart cluster"
                            icon="restart"
                            disabled={isLifecycleLocked(selectedCluster) || selectedCluster.state === 'stopped' || pendingMutations[selectedCluster.name] !== undefined}
                            onClick={() => clusterLifecycle(selectedCluster, 'restart')}
                          />
                        )}
                        <ActionButton
                          className="removeButton"
                          label={pendingMutations[selectedCluster.name] === 'delete' ? 'Deleting cluster' : 'Delete cluster'}
                          icon="remove"
                          disabled={selectedCluster.state === 'upgrading' || pendingMutations[selectedCluster.name] !== undefined}
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
                {clusterTab === 'upgrade' && (
                  <ClusterUpgrade
                    key={selectedCluster.name}
                    cluster={selectedCluster}
                    disabled={pendingMutations[selectedCluster.name] !== undefined}
                    begin={() => beginClusterMutation(selectedCluster.name, 'upgrade')}
                    end={() => endClusterMutation(selectedCluster.name)}
                    onChanged={() => { clusters.reload(); status.reload() }}
                  />
                )}
                {clusterTab === 'autoscaling' && (
                  <Suspense fallback={<section className="drawerPanel" role="status">Loading autoscaling controls...</section>}>
                    <ClusterAutoscaling
                      key={selectedCluster.name}
                      cluster={selectedCluster}
                      mutation={{
                        disabled: pendingMutations[selectedCluster.name] !== undefined,
                        begin: () => beginClusterMutation(selectedCluster.name, 'autoscaling'),
                        end: () => endClusterMutation(selectedCluster.name),
                      }}
                    />
                  </Suspense>
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
                    <select value={clusterProvider} onChange={(event) => {
                      const provider = event.target.value
                      if (provider === 'kind' || provider === 'k3s' || provider === 'k0s' || provider === 'apple') {
                        setClusterProvider(provider)
                        setClusterDatastore('')
                      }
                    }}>
                      <option value="kind">kind - Kubernetes in Porto containers</option>
                      <option value="k3s">k3s - lightweight Kubernetes on Lima VMs</option>
                      <option value="k0s">k0s - conformant Kubernetes on Lima VMs</option>
                      {providerTools.data?.some((provider) => provider.name === 'apple') && (
                        <option value="apple">Apple Cluster - native Apple silicon VMs (experimental)</option>
                      )}
                    </select>
                  </label>
                  {clusterProvider === 'k3s' && (
                    <label>
                      <span>k3s datastore</span>
                      <select value={clusterDatastore} disabled={!datastores.data?.supported || datastores.data.state === 'disabled' || !!datastores.error} onChange={(event) => setClusterDatastore(event.target.value)}>
                        <option value="">Provider default (unchanged)</option>
                        {(datastores.data?.instances ?? []).filter(guestReachableDatastore).map((instance) => <option key={instance.clusterID} value={instance.name}>Etcqlite: {instance.name}</option>)}
                      </select>
                      <small>{datastores.error || 'Optional for new k3s only. Enable, initialize and start a guest-reachable datastore in Settings first. No migration or automatic startup.'}</small>
                    </label>
                  )}
                  <label>
                    <span>Version (optional)</span>
                    <input
                      type="text"
                      value={clusterVersion}
                      placeholder={clusterProvider === 'apple' ? 'v1.35.5' : clusterProvider === 'kind' ? 'v1.37.0' : clusterProvider === 'k3s' ? 'v1.36.0+k3s1' : 'v1.36.0+k0s.0'}
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
                  {clusterProvider !== 'apple' && <label>
                    <span>Control-plane disk (GiB)</span>
                    <input type="number" min={5} value={controlPlane.diskGiB} onChange={(event) => setControlPlane({ ...controlPlane, diskGiB: Number(event.target.value) })} />
                  </label>}
                  <label>
                    <span>Initial worker nodes</span>
                    <input type="number" min={0} max={16} value={initialWorkers} onChange={(event) => setInitialWorkers(Number(event.target.value))} />
                  </label>
                  <p className="hintLine">
                    {clusterProvider === 'apple'
                      ? 'Requires macOS 26+ on Apple silicon and stable container 1.7.1+. Each directly owned node is a native VM. Graceful stop/start preserves node data and verifies recovery. Disk sizing, node scaling and in-place version upgrades are unavailable.'
                      : clusterProvider === 'kind'
                      ? 'Creates Kubernetes nodes as privileged containers through the Porto Docker endpoint.'
                      : `Creates a ${clusterProvider} control plane on a Porto-managed Lima VM. Add worker node groups afterward.`}
                  </p>
                  <button type="submit" disabled={clusterName.trim() === '' || Object.hasOwn(pendingCreations, clusterName.trim()) || (clusterProvider === 'apple' && !providerTools.data?.some((provider) => provider.name === 'apple' && provider.installed))}>Create cluster</button>
                </form>
              </Inspector>
            )}
          </div>
        </>
      )}
    </>
  )
}
