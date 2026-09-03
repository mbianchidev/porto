import { lazy, Suspense, useState } from 'react'
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

function isRunning(cluster: KubernetesCluster) {
  return cluster.state.toLowerCase() === 'running'
}

function NodeGroupTab({ cluster, onScaled }: { cluster: KubernetesCluster; onScaled: () => void }) {
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
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Scale a node group</h3>
      <p className="hintLine">Creates the group if it does not exist yet, or resizes it to the given count.</p>
      <form className="inspectorForm" onSubmit={scale}>
        <label>
          <span>Node group name</span>
          <input type="text" value={name} onChange={(event) => setName(event.target.value)} required />
        </label>
        <label>
          <span>Node count</span>
          <input type="number" min={0} max={32} value={count} onChange={(event) => setCount(Number(event.target.value))} />
        </label>
        <label>
          <span>CPUs per node</span>
          <input type="number" min={1} max={32} value={machine.cpus} onChange={(event) => setMachine({ ...machine, cpus: Number(event.target.value) })} />
        </label>
        <label>
          <span>Memory per node (MiB)</span>
          <input type="number" min={512} step={512} value={machine.memoryMiB} onChange={(event) => setMachine({ ...machine, memoryMiB: Number(event.target.value) })} />
        </label>
        <label>
          <span>Disk per node (GiB)</span>
          <input type="number" min={5} value={machine.diskGiB} onChange={(event) => setMachine({ ...machine, diskGiB: Number(event.target.value) })} />
        </label>
        <label>
          <span>k3s version (optional)</span>
          <input type="text" value={version} placeholder="v1.30.4+k3s1" onChange={(event) => setVersion(event.target.value)} />
        </label>
        <button type="submit" disabled={submitting || name.trim() === ''}>{submitting ? 'Scaling…' : 'Scale node group'}</button>
      </form>
    </section>
  )
}

function ImportImageTab({ cluster }: { cluster: KubernetesCluster }) {
  const { notifyError, notifyNotice } = useMessages()
  const [image, setImage] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function importImage(event: FormEvent) {
    event.preventDefault()
    if (image.trim() === '') return
    setSubmitting(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${cluster.name}/images/import`, 'POST', { image: image.trim() })
      notifyNotice('kubernetes', `Imported ${image.trim()} into ${cluster.name}.`)
      setImage('')
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to import ${image}`))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Import a container image</h3>
      <p className="hintLine">Loads a local or pulled image directly into the cluster's containerd, skipping a registry round trip.</p>
      <form className="inspectorForm" onSubmit={importImage}>
        <label>
          <span>Image reference</span>
          <input type="text" value={image} placeholder="registry/repository:tag" onChange={(event) => setImage(event.target.value)} required />
        </label>
        <button type="submit" disabled={submitting || image.trim() === ''}>{submitting ? 'Importing…' : 'Import image'}</button>
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
  const [submittingCluster, setSubmittingCluster] = useState(false)
  const [installingProvider, setInstallingProvider] = useState<string | null>(null)
  const [renameDraft, setRenameDraft] = useState('')
  const [renamingCluster, setRenamingCluster] = useState(false)
  const [clusterCreateStatus, setClusterCreateStatus] = useState('')
  const [clusterCreateError, setClusterCreateError] = useState('')

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

  async function createCluster(event: FormEvent) {
    event.preventDefault()
    const requestedName = clusterName.trim()
    const requestedProvider = clusterProvider
    if (requestedName === '') return
    const progress = `Creating ${requestedProvider} cluster ${requestedName}. Provisioning nodes and add-ons can take several minutes.`
    setClusterCreateError('')
    setClusterCreateStatus(progress)
    setSubmittingCluster(true)
    recordActivity('info', 'kubernetes', progress)
    try {
      const cluster = await apiSend<KubernetesCluster>('/api/kubernetes/clusters', 'POST', {
        name: requestedName,
        provider: requestedProvider,
        version: clusterVersion,
        controlPlane,
        nodeGroups: initialWorkers > 0
          ? [{ name: 'workers', count: initialWorkers, machine: DEFAULT_MACHINE, labels: {}, taints: [] }]
          : [],
      })
      notifyNotice('kubernetes', `Created ${cluster.provider} cluster ${cluster.name}.`)
      onContextChange(cluster.context)
      setSelectedClusterName(cluster.name)
      setRenameDraft(cluster.name)
      setClusterName('')
      setClusterVersion('')
      setClusterProvider('k3s')
      setControlPlane(DEFAULT_MACHINE)
      setInitialWorkers(1)
      setClusterCreateStatus('')
      setCreatingCluster(false)
      clusters.reload()
    } catch (err) {
      const message = errorMessage(err, `Unable to create cluster ${requestedName}`)
      setClusterCreateStatus('')
      setClusterCreateError(message)
      notifyError('kubernetes', message)
    } finally {
      setSubmittingCluster(false)
    }
  }

  async function clusterLifecycle(cluster: KubernetesCluster, action: 'start' | 'stop') {
    try {
      const result = await apiSend<{ status: string; message?: string }>(
        `/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/${action}`,
        'POST',
      )
      notifyNotice('kubernetes', result.message || `${cluster.name} ${action === 'start' ? 'starting' : 'stopping'}.`)
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
    }
  }

  async function deleteCluster(cluster: KubernetesCluster) {
    if (!window.confirm(`Delete cluster ${cluster.name}? This permanently removes its control-plane and worker nodes.`)) return
    try {
      await apiSend(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}?confirm=true`, 'DELETE')
      notifyNotice('kubernetes', `Deleted cluster ${cluster.name}.`)
      if (selectedClusterName === cluster.name) setSelectedClusterName(null)
      clusters.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to delete ${cluster.name}`))
    }
  }

  async function renameCluster(event: FormEvent) {
    event.preventDefault()
    if (!selectedCluster || renameDraft.trim() === '' || renameDraft.trim() === selectedCluster.name) return
    setRenamingCluster(true)
    try {
      const previousName = selectedCluster.name
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
      setRenamingCluster(false)
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
        }}>{submittingCluster ? 'View creation' : 'New cluster'}</button>
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
              columnsTemplate="12px minmax(130px,1fr) minmax(80px,0.5fr) minmax(90px,0.6fr) minmax(140px,1fr) minmax(140px,1fr) minmax(55px,0.35fr)"
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
                { header: 'Context', className: 'mono', render: (cluster) => cluster.context },
                { header: 'Server', className: 'mono', render: (cluster) => cluster.server || '—' },
                { header: 'Nodes', className: 'mono', render: (cluster) => cluster.nodes?.length ?? 0 },
              ]}
              renderActions={(cluster) => (
                isRunning(cluster)
                  ? <ActionButton label="Stop cluster" icon="stop" onClick={() => clusterLifecycle(cluster, 'stop')} />
                  : <ActionButton label="Start cluster" icon="play" disabled={cluster.state === 'orphaned'} onClick={() => clusterLifecycle(cluster, 'start')} />
              )}
            />

            {selectedCluster && !creatingCluster && (
              <Inspector title={selectedCluster.name} subtitle={selectedCluster.context} onClose={() => setSelectedClusterName(null)}>
                <InspectorTabs
                  tabs={selectedCluster.state === 'orphaned'
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
                    {selectedCluster.state === 'broken' && (
                      <p className="errorLine">The control-plane node is missing. Starting a KinD cluster will recreate it from the saved cluster configuration.</p>
                    )}
                    <dl className="runtimeGrid">
                      <div><dt>Context</dt><dd>{selectedCluster.context}</dd></div>
                      <div><dt>Provider</dt><dd>{selectedCluster.provider}</dd></div>
                      <div><dt>State</dt><dd>{selectedCluster.state}</dd></div>
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
                          disabled={selectedCluster.state === 'orphaned' || renamingCluster}
                          onChange={(event) => setRenameDraft(event.target.value)}
                          required
                        />
                      </label>
                      <button
                        type="submit"
                        disabled={selectedCluster.state === 'orphaned' || renamingCluster || renameDraft.trim() === '' || renameDraft.trim() === selectedCluster.name}
                      >
                        {renamingCluster ? 'Renaming…' : 'Rename cluster'}
                      </button>
                    </form>
                    <div className="maintenanceBar">
                      <span>Maintenance controls</span>
                      <div className="actions">
                        {isRunning(selectedCluster)
                          ? <ActionButton label="Stop cluster" icon="stop" onClick={() => clusterLifecycle(selectedCluster, 'stop')} />
                          : <ActionButton label="Start cluster" icon="play" disabled={selectedCluster.state === 'orphaned'} onClick={() => clusterLifecycle(selectedCluster, 'start')} />}
                        <ActionButton className="removeButton" label="Delete cluster" icon="remove" onClick={() => deleteCluster(selectedCluster)} />
                      </div>
                    </div>
                  </section>
                )}
                {clusterTab === 'terminal' && (
                  <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading k9s terminal…</div></section>}>
                    <ClusterTerminal key={`${selectedCluster.name}:${selectedCluster.state}`} cluster={selectedCluster} />
                  </Suspense>
                )}
                {clusterTab === 'nodeGroup' && <NodeGroupTab cluster={selectedCluster} onScaled={clusters.reload} />}
                {clusterTab === 'importImage' && <ImportImageTab cluster={selectedCluster} />}
              </Inspector>
            )}

            {creatingCluster && (
              <Inspector title="New cluster" subtitle="Managed by Porto" onClose={() => setCreatingCluster(false)}>
                <form className="inspectorForm" onSubmit={createCluster}>
                  {clusterCreateStatus && <p className="hintLine" role="status" aria-live="polite">{clusterCreateStatus}</p>}
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
                  <button type="submit" disabled={submittingCluster || clusterName.trim() === ''}>{submittingCluster ? 'Creating…' : 'Create cluster'}</button>
                </form>
              </Inspector>
            )}
          </div>
        </>
      )}
    </>
  )
}
