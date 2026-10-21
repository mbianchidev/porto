import { useState, type FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { KubernetesAutoscalingConfig, KubernetesAutoscalingStatus, KubernetesCluster } from '../types'

type Mutation = { disabled: boolean; begin: () => boolean; end: () => void }

function AutoscalingSettings({ cluster, status, mutation, onChanged }: {
  cluster: KubernetesCluster
  status: KubernetesAutoscalingStatus
  mutation: Mutation
  onChanged: () => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [config, setConfig] = useState<KubernetesAutoscalingConfig>({
    nodes: {
      enabled: status.nodes.enabled,
      maxNodes: status.nodes.maxNodes || 3,
      machine: {
        cpus: status.nodes.machine.cpus || 2,
        memoryMiB: status.nodes.machine.memoryMiB || 2048,
        diskGiB: status.nodes.machine.diskGiB || 20,
      },
    },
    keda: status.keda,
  })
  const [saving, setSaving] = useState(false)
  const locked = mutation.disabled || saving || cluster.state !== 'running'
  const machine = config.nodes.machine

  async function save(event: FormEvent) {
    event.preventDefault()
    if (!mutation.begin()) return
    setSaving(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/autoscaling`, 'POST', config)
      notifyNotice('kubernetes', `Autoscaling configuration saved for ${cluster.name}.`)
      onChanged()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, 'Unable to configure cluster autoscaling'))
      onChanged()
    } finally {
      setSaving(false)
      mutation.end()
    }
  }

  return (
    <section className="drawerPanel clusterMaintenance">
      <h3>Cluster autoscaling</h3>
      <p>Karpenter supplies real worker VMs when pods cannot be scheduled. KEDA scales workload replicas from event triggers. The control plane and fixed worker groups remain independent.</p>
      {status.message && <p className="hintLine" role="status">{status.message}</p>}
      {status.nodes.enabled && !status.running && !status.paused && <p className="errorLine" role="alert">The node controller is not running. Save the configuration to resume it, or check daemon diagnostics.</p>}
      <dl className="runtimeGrid">
        <div><dt>Karpenter</dt><dd>{status.karpenterVersion} / {status.running ? 'Running' : status.paused ? 'Paused' : 'Stopped'}</dd></div>
        <div><dt>Reserved workers</dt><dd>{status.managedNodes}</dd></div>
        <div><dt>KEDA</dt><dd>{status.kedaVersion} / {status.kedaReady ? 'Ready' : status.keda ? 'Not ready' : 'Disabled'}</dd></div>
      </dl>
      <form className="inspectorForm" onSubmit={save}>
        <label className="checkboxRow">
          <input type="checkbox" checked={config.nodes.enabled} disabled={locked || !status.nodeSupported} onChange={(event) => setConfig({ ...config, nodes: { ...config.nodes, enabled: event.target.checked } })} />
          <span>Autoscale worker nodes with Karpenter</span>
        </label>
        {!status.nodeSupported && <p className="hintLine">kind fixes node topology at creation. Use k3s or k0s for real local node autoscaling; KEDA is supported on kind.</p>}
        {config.nodes.enabled && (
          <>
            <label><span>Maximum autoscaled workers</span><input type="number" min={1} max={32} required disabled={locked} value={config.nodes.maxNodes} onChange={(event) => setConfig({ ...config, nodes: { ...config.nodes, maxNodes: Number(event.target.value) } })} /></label>
            <label><span>CPUs per autoscaled worker</span><input type="number" min={1} max={32} required disabled={locked} value={machine.cpus} onChange={(event) => setConfig({ ...config, nodes: { ...config.nodes, machine: { ...machine, cpus: Number(event.target.value) } } })} /></label>
            <label><span>Worker memory (MiB)</span><input type="number" min={1024} max={131072} step={512} required disabled={locked} value={machine.memoryMiB} onChange={(event) => setConfig({ ...config, nodes: { ...config.nodes, machine: { ...machine, memoryMiB: Number(event.target.value) } } })} /></label>
            <label><span>Worker disk (GiB)</span><input type="number" min={10} max={1024} required disabled={locked} value={machine.diskGiB} onChange={(event) => setConfig({ ...config, nodes: { ...config.nodes, machine: { ...machine, diskGiB: Number(event.target.value) } } })} /></label>
            <p className="hintLine">Worker ceiling: {config.nodes.maxNodes * machine.cpus} CPUs, {config.nodes.maxNodes * machine.memoryMiB} MiB RAM, {config.nodes.maxNodes * machine.diskGiB} GiB disk. Limits exclude fixed nodes. Empty workers consolidate only when storage is safe.</p>
          </>
        )}
        <label className="checkboxRow">
          <input type="checkbox" checked={config.keda} disabled={locked} onChange={(event) => setConfig({ ...config, keda: event.target.checked })} />
          <span>Enable KEDA event-driven workload scaling</span>
        </label>
        <p className="hintLine">Disabling node scaling leaves existing VMs intact. KEDA cannot be disabled while workload scalers remain. Porto will not take over a foreign installation.</p>
        <button type="submit" disabled={locked}>{saving ? 'Configuring autoscaling...' : 'Save autoscaling'}</button>
      </form>
    </section>
  )
}

function WorkloadScaling({ cluster, ready, mutation, onChanged }: {
  cluster: KubernetesCluster
  ready: boolean
  mutation: Mutation
  onChanged: () => void
}) {
  const { notifyError, notifyNotice } = useMessages()
  const [namespace, setNamespace] = useState('default')
  const [name, setName] = useState('')
  const [targetName, setTargetName] = useState('')
  const [targetKind, setTargetKind] = useState('Deployment')
  const [minReplicas, setMinReplicas] = useState(0)
  const [maxReplicas, setMaxReplicas] = useState(5)
  const [triggers, setTriggers] = useState('')
  const [working, setWorking] = useState(false)
  const locked = mutation.disabled || working || !ready || cluster.state !== 'running'

  async function apply(event: FormEvent) {
    event.preventDefault()
    let parsed: unknown
    try {
      parsed = JSON.parse(triggers)
      if (!Array.isArray(parsed) || parsed.length === 0) throw new Error('Triggers must be a nonempty JSON array.')
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, 'Invalid KEDA trigger JSON'))
      return
    }
    if (!mutation.begin()) return
    setWorking(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/autoscaling/workloads`, 'POST', {
        namespace: namespace.trim(), name: name.trim(), targetName: targetName.trim(),
        targetKind, targetAPIVersion: 'apps/v1', minReplicas, maxReplicas, triggers: parsed,
      })
      notifyNotice('kubernetes', `Configured KEDA scaler ${namespace}/${name}.`)
      onChanged()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, 'Unable to configure workload scaling'))
    } finally {
      setWorking(false)
      mutation.end()
    }
  }

  async function remove() {
    if (!window.confirm(`Remove KEDA scaler ${namespace}/${name}? The workload remains, but event-driven replica control stops.`) || !mutation.begin()) return
    setWorking(true)
    try {
      await apiSend(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/autoscaling/workloads/${encodeURIComponent(namespace.trim())}/${encodeURIComponent(name.trim())}?confirm=true`, 'DELETE')
      notifyNotice('kubernetes', `Removed KEDA scaler ${namespace}/${name}.`)
      onChanged()
    } catch (err) {
      notifyError('kubernetes', errorMessage(err, 'Unable to remove workload scaling'))
    } finally {
      setWorking(false)
      mutation.end()
    }
  }

  return (
    <section className="drawerPanel clusterMaintenance">
      <h3>Event-driven workload</h3>
      {!ready && <p className="hintLine">Enable KEDA and wait for its controllers to become Ready before configuring a workload.</p>}
      <form className="inspectorForm" onSubmit={apply}>
        <label><span>Namespace</span><input type="text" required value={namespace} disabled={locked} onChange={(event) => setNamespace(event.target.value)} /></label>
        <label><span>Scaler name</span><input type="text" required value={name} disabled={locked} onChange={(event) => setName(event.target.value)} /></label>
        <label><span>Target workload</span><input type="text" required value={targetName} disabled={locked} onChange={(event) => setTargetName(event.target.value)} /></label>
        <label><span>Workload kind</span><select value={targetKind} disabled={locked} onChange={(event) => setTargetKind(event.target.value)}><option>Deployment</option><option>StatefulSet</option></select></label>
        <label><span>Minimum replicas</span><input type="number" min={0} max={maxReplicas} required value={minReplicas} disabled={locked} onChange={(event) => setMinReplicas(Number(event.target.value))} /></label>
        <label><span>Maximum replicas</span><input type="number" min={Math.max(1, minReplicas)} max={10000} required value={maxReplicas} disabled={locked} onChange={(event) => setMaxReplicas(Number(event.target.value))} /></label>
        <label><span>KEDA triggers (JSON array)</span><textarea rows={7} required value={triggers} disabled={locked} onChange={(event) => setTriggers(event.target.value)} placeholder={'[{"type":"cron","metadata":{"timezone":"Etc/UTC","start":"0 8 * * *","end":"0 18 * * *","desiredReplicas":"2"}}]'} /></label>
        <p className="hintLine">Reference existing TriggerAuthentication resources for credentials. Do not paste secrets into trigger metadata. One scaler owns each workload's replicas.</p>
        <button type="submit" disabled={locked}>Apply workload scaling</button>
        <button type="button" disabled={locked || namespace.trim() === '' || name.trim() === ''} onClick={() => void remove()}>Remove named scaler</button>
      </form>
    </section>
  )
}

export default function ClusterAutoscaling({ cluster, mutation }: { cluster: KubernetesCluster; mutation: Mutation }) {
  const status = usePolledResource<KubernetesAutoscalingStatus>(
    (signal) => apiGet(`/api/kubernetes/clusters/${encodeURIComponent(cluster.name)}/autoscaling`, signal),
    10000, [], `kubernetes:autoscaling:${cluster.name}`,
  )
  if (status.error) return <section className="drawerPanel"><p className="errorLine" role="alert">{status.error}</p><button type="button" onClick={status.reload}>Retry autoscaling status</button></section>
  if (!status.data) return <section className="drawerPanel" role="status">Loading autoscaling configuration...</section>
  const current = status.data
  return (
    <>
      <AutoscalingSettings key={`${cluster.name}:${JSON.stringify({ nodes: current.nodes, keda: current.keda })}`} cluster={cluster} status={current} mutation={mutation} onChanged={status.reload} />
      <WorkloadScaling cluster={cluster} ready={current.keda && current.kedaReady} mutation={mutation} onChanged={status.reload} />
    </>
  )
}
