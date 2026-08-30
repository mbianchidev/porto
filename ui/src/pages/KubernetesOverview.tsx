import { useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type {
  KubernetesCluster,
  KubernetesContext,
  KubernetesMachineSpec,
  KubernetesStatus,
} from '../types'

const DEFAULT_MACHINE: KubernetesMachineSpec = { cpus: 2, memoryMiB: 2048, diskGiB: 20 }

function NodeGroupTab({ cluster, onScaled }: { cluster: KubernetesCluster; onScaled: () => void }) {
  const { notifyError, notifyNotice } = useMessages()
  const [name, setName] = useState('workers')
  const [count, setCount] = useState(1)
  const [machine, setMachine] = useState<KubernetesMachineSpec>(DEFAULT_MACHINE)
  const [version, setVersion] = useState('')
  const [submitting, setSubmitting] = useState(false)

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
  onContextChange,
}: {
  context: string
  onContextChange: (context: string) => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [selectedClusterName, setSelectedClusterName] = useState<string | null>(null)
  const [clusterTab, setClusterTab] = useState('overview')
  const [creatingCluster, setCreatingCluster] = useState(false)
  const [clusterName, setClusterName] = useState('')
  const [clusterVersion, setClusterVersion] = useState('')
  const [controlPlane, setControlPlane] = useState<KubernetesMachineSpec>(DEFAULT_MACHINE)
  const [submittingCluster, setSubmittingCluster] = useState(false)

  const status = usePolledResource<KubernetesStatus>(
    (signal) => apiGet(`/api/kubernetes/status?context=${encodeURIComponent(context)}`, signal),
    10000,
    [context],
  )
  const contexts = usePolledResource<KubernetesContext[]>((signal) => apiGet('/api/kubernetes/contexts', signal), 15000, [])
  const clusters = usePolledResource<KubernetesCluster[]>((signal) => apiGet('/api/kubernetes/clusters', signal), 10000, [])
  const items = contexts.data ?? []
  const clusterItems = clusters.data ?? []
  const selectedCluster = clusterItems.find((cluster) => cluster.name === selectedClusterName) ?? null
  const available = status.data?.available ?? false
  const enabled = status.data?.enabled ?? false

  async function createCluster(event: FormEvent) {
    event.preventDefault()
    if (clusterName.trim() === '') return
    setSubmittingCluster(true)
    try {
      const cluster = await apiSend<KubernetesCluster>('/api/kubernetes/clusters', 'POST', {
        name: clusterName.trim(),
        version: clusterVersion,
        controlPlane,
        nodeGroups: [],
      })
      notifyNotice('kubernetes', `Provisioning cluster ${clusterName.trim()}. Add node groups from its inspector once it is ready.`)
      onContextChange(cluster.context)
      setSelectedClusterName(cluster.name)
      setClusterName('')
      setClusterVersion('')
      setControlPlane(DEFAULT_MACHINE)
      setCreatingCluster(false)
      clusters.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to create cluster ${clusterName}`))
    } finally {
      setSubmittingCluster(false)
    }
  }

  async function clusterLifecycle(cluster: KubernetesCluster, action: 'start' | 'stop') {
    try {
      await apiSend(`/api/kubernetes/clusters/${cluster.name}/${action}`, 'POST')
      notifyNotice('kubernetes', `${cluster.name} ${action === 'start' ? 'starting' : 'stopping'}.`)
      clusters.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to ${action} ${cluster.name}`))
    }
  }

  async function deleteCluster(cluster: KubernetesCluster) {
    if (!window.confirm(`Delete cluster ${cluster.name}? This removes its control-plane and worker VMs permanently.`)) return
    try {
      await apiSend(`/api/kubernetes/clusters/${cluster.name}?confirm=true`, 'DELETE')
      notifyNotice('kubernetes', `Deleted cluster ${cluster.name}.`)
      if (selectedClusterName === cluster.name) setSelectedClusterName(null)
      clusters.reload()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, `Unable to delete ${cluster.name}`))
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Kubernetes status">
        <span className="fleetRailTitle">Cluster signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetDatum"><small>Context</small><strong>{status.data?.context || context || 'default'}</strong></span>
        {status.data?.serverVersion && <span className="fleetDatum"><small>Server</small><strong>{status.data.serverVersion}</strong></span>}
        <span className="fleetMessage">{items.length} configured context(s) · {clusterItems.length} Porto cluster(s)</span>
      </section>
      <div className="controlBar">
        <span className="filterResultCount" aria-live="polite">Active context: {context || 'cluster default'}</span>
        <button className="refreshControl" type="button" onClick={() => { status.reload(); contexts.reload(); clusters.reload() }}>Refresh</button>
        <button type="button" disabled={!enabled} onClick={() => { setSelectedClusterName(null); setCreatingCluster(true) }}>New cluster</button>
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
            emptyMessage={contexts.error || 'No kubeconfig contexts found.'}
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
              columnsTemplate="minmax(150px,1.2fr) minmax(160px,1.2fr) minmax(160px,1.2fr) minmax(70px,0.5fr)"
              selectedKey={selectedClusterName}
              onSelect={(cluster) => { setCreatingCluster(false); setSelectedClusterName(cluster.name); setClusterTab('overview') }}
              ariaLabel="Porto-provisioned Kubernetes clusters"
              emptyMessage={clusters.error || 'No Porto-provisioned clusters yet. Create one to get started.'}
              columns={[
                { header: 'Name', render: (cluster) => <strong>{cluster.name}</strong> },
                { header: 'Context', className: 'mono', render: (cluster) => cluster.context },
                { header: 'Server', className: 'mono', render: (cluster) => cluster.server || '—' },
                { header: 'Nodes', className: 'mono', render: (cluster) => cluster.nodes?.length ?? 0 },
              ]}
              renderActions={(cluster) => (
                <>
                  <ActionButton label="Start cluster" icon="play" onClick={() => clusterLifecycle(cluster, 'start')} />
                  <ActionButton label="Stop cluster" icon="stop" onClick={() => clusterLifecycle(cluster, 'stop')} />
                </>
              )}
            />

            {selectedCluster && !creatingCluster && (
              <Inspector title={selectedCluster.name} subtitle={selectedCluster.context} onClose={() => setSelectedClusterName(null)}>
                <InspectorTabs
                  tabs={[{ id: 'overview', label: 'Overview' }, { id: 'nodeGroup', label: 'Node group' }, { id: 'importImage', label: 'Import image' }]}
                  activeID={clusterTab}
                  onSelect={setClusterTab}
                />
                {clusterTab === 'overview' && (
                  <section className="drawerPanel">
                    <h3>Cluster detail</h3>
                    <dl className="runtimeGrid">
                      <div><dt>Context</dt><dd>{selectedCluster.context}</dd></div>
                      <div><dt>Server</dt><dd>{selectedCluster.server || '—'}</dd></div>
                      <div><dt>Kubeconfig</dt><dd>{selectedCluster.kubeconfigPath}</dd></div>
                      <div><dt>Nodes</dt><dd>{selectedCluster.nodes?.join(', ') || '—'}</dd></div>
                    </dl>
                    <div className="maintenanceBar">
                      <span>Maintenance controls</span>
                      <div className="actions">
                        <ActionButton label="Start cluster" icon="play" onClick={() => clusterLifecycle(selectedCluster, 'start')} />
                        <ActionButton label="Stop cluster" icon="stop" onClick={() => clusterLifecycle(selectedCluster, 'stop')} />
                        <ActionButton className="removeButton" label="Delete cluster" icon="remove" onClick={() => deleteCluster(selectedCluster)} />
                      </div>
                    </div>
                  </section>
                )}
                {clusterTab === 'nodeGroup' && <NodeGroupTab cluster={selectedCluster} onScaled={clusters.reload} />}
                {clusterTab === 'importImage' && <ImportImageTab cluster={selectedCluster} />}
              </Inspector>
            )}

            {creatingCluster && (
              <Inspector title="New cluster" subtitle="k3s on Lima VMs" onClose={() => setCreatingCluster(false)}>
                <form className="inspectorForm" onSubmit={createCluster}>
                  <label>
                    <span>Name</span>
                    <input type="text" value={clusterName} placeholder="dev-cluster" onChange={(event) => setClusterName(event.target.value)} required />
                  </label>
                  <label>
                    <span>k3s version (optional)</span>
                    <input type="text" value={clusterVersion} placeholder="v1.30.4+k3s1" onChange={(event) => setClusterVersion(event.target.value)} />
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
                  <p className="hintLine">Creates a single-node k3s control plane on a new Lima VM. Add worker node groups afterward from the cluster's inspector.</p>
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
