import { lazy, Suspense, useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector, InspectorTabs } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type { RuntimeProviderStatus, VMCreateRequest, VMImage, VMInstance, VMStatus } from '../types'

const COLUMNS_TEMPLATE = '12px minmax(150px,1.2fr) minmax(110px,0.7fr) minmax(70px,0.4fr) minmax(90px,0.5fr) minmax(90px,0.5fr)'
const VMTerminal = lazy(() => import('../components/VMTerminal'))

function formatBytes(bytes: number): string {
  if (!bytes) return '—'
  const gib = bytes / (1024 * 1024 * 1024)
  return `${gib.toFixed(gib >= 10 ? 0 : 1)} GiB`
}

function SnapshotTab({ instance }: { instance: VMInstance }) {
  const { notifyError, notifyNotice } = useMessages()
  const [snapshotName, setSnapshotName] = useState('')
  const [busy, setBusy] = useState(false)

  async function createSnapshot() {
    if (snapshotName.trim() === '') return
    setBusy(true)
    try {
      await apiSend(`/api/vms/instances/${instance.name}/snapshot`, 'POST', { name: snapshotName.trim() })
      notifyNotice('machines', `Created snapshot ${snapshotName.trim()} for ${instance.name}.`)
      setSnapshotName('')
    } catch (err) {
      notifyError('machines', errorMessage(err, 'Unable to create snapshot'))
    } finally {
      setBusy(false)
    }
  }

  async function restoreSnapshot() {
    if (snapshotName.trim() === '') return
    if (!window.confirm(`Restore ${instance.name} to snapshot ${snapshotName.trim()}? Unsaved state will be lost.`)) return
    setBusy(true)
    try {
      await apiSend(`/api/vms/instances/${instance.name}/restore`, 'POST', { name: snapshotName.trim() })
      notifyNotice('machines', `Restored ${instance.name} to snapshot ${snapshotName.trim()}.`)
    } catch (err) {
      notifyError('machines', errorMessage(err, 'Unable to restore snapshot'))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="drawerPanel">
      <h3>Snapshots</h3>
      <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); createSnapshot() }}>
        <label>
          <span>Snapshot name</span>
          <input type="text" value={snapshotName} placeholder="pre-upgrade" onChange={(event) => setSnapshotName(event.target.value)} />
        </label>
        <div className="actions">
          <button type="submit" disabled={busy || snapshotName.trim() === ''}>Create snapshot</button>
          <button className="destructiveAction" type="button" disabled={busy || snapshotName.trim() === ''} onClick={restoreSnapshot}>Restore snapshot</button>
        </div>
      </form>
    </section>
  )
}

export function Machines() {
  const { notifyError, notifyNotice } = useMessages()
  const [selectedName, setSelectedName] = useState<string | null>(null)
  const [tab, setTab] = useState('overview')
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<VMCreateRequest>({
    name: '', image: '', cpus: 2, memoryMiB: 2048, diskGiB: 20, architecture: '', provision: '', start: true,
  })
  const [submitting, setSubmitting] = useState(false)
  const [installing, setInstalling] = useState(false)

  const status = usePolledResource<VMStatus>((signal) => apiGet('/api/vms/status', signal), 10000, [])
  const images = usePolledResource<VMImage[]>((signal) => apiGet('/api/vms/images', signal), 0, [])
  const instances = usePolledResource<VMInstance[]>((signal) => apiGet('/api/vms/instances', signal), 6000, [])
  const providers = usePolledResource<RuntimeProviderStatus[]>((signal) => apiGet('/api/runtime/providers', signal), 15000, [])
  const items = instances.data ?? []
  const selected = items.find((instance) => instance.name === selectedName) ?? null
  const available = status.data?.available ?? false
  const lima = providers.data?.find((provider) => provider.name === 'lima')
  const selectedImage = images.data?.find((image) => image.id === form.image)
  const isRunning = (instance: VMInstance) => instance.status.toLocaleLowerCase() === 'running'

  async function createInstance(event: FormEvent) {
    event.preventDefault()
    if (form.name.trim() === '' || form.image === '') return
    setSubmitting(true)
    try {
      await apiSend('/api/vms/instances', 'POST', form)
      notifyNotice('machines', `Created VM ${form.name.trim()}.`)
      setForm({ name: '', image: '', cpus: 2, memoryMiB: 2048, diskGiB: 20, architecture: '', provision: '', start: true })
      setCreating(false)
      instances.reload()
    } catch (err) {
      notifyError('machines', errorMessage(err, `Unable to create VM ${form.name}`))
    } finally {
      setSubmitting(false)
    }
  }

  async function lifecycle(instance: VMInstance, action: 'start' | 'stop') {
    try {
      await apiSend(`/api/vms/instances/${instance.name}/${action}`, 'POST')
      notifyNotice('machines', `${instance.name} ${action === 'start' ? 'starting' : 'stopping'}.`)
      instances.reload()
    } catch (err) {
      notifyError('machines', errorMessage(err, `Unable to ${action} ${instance.name}`))
    }
  }

  async function deleteInstance(instance: VMInstance) {
    if (!window.confirm(`Delete VM ${instance.name}? This removes its disk permanently.`)) return
    try {
      await apiSend(`/api/vms/instances/${instance.name}?confirm=true`, 'DELETE')
      notifyNotice('machines', `Deleted VM ${instance.name}.`)
      if (selectedName === instance.name) setSelectedName(null)
      instances.reload()
    } catch (err) {
      notifyError('machines', errorMessage(err, `Unable to delete ${instance.name}`))
    }
  }

  async function installLima() {
    setInstalling(true)
    try {
      await apiSend('/api/runtime/providers/lima/install', 'POST')
      notifyNotice('machines', 'Lima provider installed.')
      providers.reload()
      status.reload()
    } catch (err) {
      notifyError('machines', errorMessage(err, 'Unable to install Lima'))
    } finally {
      setInstalling(false)
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Virtual machine status">
        <span className="fleetRailTitle">VM signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        {status.data?.provider && <span className="fleetDatum"><small>Provider</small><strong>{status.data.provider}</strong></span>}
        <span className="fleetMessage">{items.length} instance(s)</span>
      </section>
      <div className="controlBar">
        <span className="filterResultCount" aria-live="polite">{items.length} instance(s) · {(images.data ?? []).length} image(s) catalogued</span>
        <button className="refreshControl" type="button" onClick={instances.reload}>Refresh</button>
        <button type="button" disabled={!available} onClick={() => { setSelectedName(null); setCreating(true) }}>New machine</button>
      </div>
      <div className="workArea">
        {!available ? (
          status.data?.enabled && lima && !lima.installed ? (
            <article className="empty unavailableNotice" role="status">
              <h2>Virtual machine provider is missing</h2>
              <p>{lima.message || 'Install Lima to create and manage Linux virtual machines.'}</p>
              <button type="button" disabled={installing} onClick={installLima}>{installing ? 'Installing Lima…' : 'Install Lima'}</button>
            </article>
          ) : (
            <RuntimeGate
              label="Virtual machines"
              enabled={status.data?.enabled ?? false}
              message={status.data?.message || status.error || (status.data?.enabled ? 'Porto could not reach Lima.' : undefined)}
            />
          )
        ) : (
          <InventoryList
            items={items}
            getKey={(instance) => instance.name}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(instance) => lampStateFor(instance.status)}
            getLampLabel={(instance) => instance.status}
            selectedKey={selectedName}
            onSelect={(instance) => { setCreating(false); setSelectedName(instance.name); setTab('overview') }}
            ariaLabel="Virtual machines"
            emptyMessage={instances.error || 'No virtual machines yet. Create one from the image catalog.'}
            columns={[
              { header: 'Name', render: (instance) => <strong>{instance.name}</strong> },
              { header: 'Arch', className: 'mono', render: (instance) => instance.architecture },
              { header: 'CPUs', className: 'mono', render: (instance) => instance.cpus },
              { header: 'Memory', className: 'mono', render: (instance) => formatBytes(instance.memoryBytes) },
              { header: 'Disk', className: 'mono', render: (instance) => formatBytes(instance.diskBytes) },
            ]}
            renderActions={(instance) => (
              isRunning(instance)
                ? <ActionButton label="Stop VM" icon="stop" onClick={() => lifecycle(instance, 'stop')} />
                : <ActionButton label="Start VM" icon="play" onClick={() => lifecycle(instance, 'start')} />
            )}
          />
        )}

        {selected && !creating && (
          <Inspector title={selected.name} subtitle={selected.status} onClose={() => setSelectedName(null)}>
            <InspectorTabs
              tabs={[{ id: 'overview', label: 'Overview' }, { id: 'terminal', label: 'Terminal' }, { id: 'snapshots', label: 'Snapshots' }]}
              activeID={tab}
              onSelect={setTab}
            />
            {tab === 'overview' && (
              <section className="drawerPanel">
                <h3>Instance detail</h3>
                <dl className="runtimeGrid">
                  <div><dt>Architecture</dt><dd>{selected.architecture}</dd></div>
                  <div><dt>CPUs</dt><dd>{selected.cpus}</dd></div>
                  <div><dt>Memory</dt><dd>{formatBytes(selected.memoryBytes)}</dd></div>
                  <div><dt>Disk</dt><dd>{formatBytes(selected.diskBytes)}</dd></div>
                  <div><dt>SSH port</dt><dd>{selected.sshLocalPort || '—'}</dd></div>
                  <div><dt>Directory</dt><dd>{selected.directory || '—'}</dd></div>
                  <div><dt>Addresses</dt><dd>{selected.addresses?.join(', ') || '—'}</dd></div>
                </dl>
                <div className="maintenanceBar">
                  <span>Maintenance controls</span>
                  <div className="actions">
                    {isRunning(selected)
                      ? <ActionButton label="Stop VM" icon="stop" onClick={() => lifecycle(selected, 'stop')} />
                      : <ActionButton label="Start VM" icon="play" onClick={() => lifecycle(selected, 'start')} />}
                    <ActionButton className="removeButton" label="Delete VM" icon="remove" onClick={() => deleteInstance(selected)} />
                  </div>
                </div>
              </section>
            )}
            {tab === 'terminal' && (
              <Suspense fallback={<section className="logConsole vmTerminal"><div className="terminalPlaceholder">Loading terminal…</div></section>}>
                <VMTerminal key={`${selected.name}:${isRunning(selected)}`} instance={selected} />
              </Suspense>
            )}
            {tab === 'snapshots' && <SnapshotTab instance={selected} />}
          </Inspector>
        )}

        {creating && (
          <Inspector title="New machine" subtitle="limactl create" onClose={() => setCreating(false)}>
            <form className="inspectorForm" onSubmit={createInstance}>
              <label>
                <span>Name</span>
                <input type="text" value={form.name} placeholder="dev-box" onChange={(event) => setForm({ ...form, name: event.target.value })} required />
              </label>
              <label>
                <span>Distribution</span>
                <select value={form.image} onChange={(event) => setForm({ ...form, image: event.target.value })} required>
                  <option value="" disabled>Choose an image</option>
                  {(images.data ?? []).map((image) => (
                    <option key={image.id} value={image.id} disabled={!image.available}>
                      {image.distribution} · {image.version}{image.available ? '' : ' · unavailable'}
                    </option>
                  ))}
                </select>
              </label>
              {selectedImage?.message && <p className="errorLine">{selectedImage.message}</p>}
              <label>
                <span>Architecture</span>
                <select value={form.architecture} onChange={(event) => setForm({ ...form, architecture: event.target.value })}>
                  <option value="">Host default</option>
                  <option value="aarch64">aarch64</option>
                  <option value="x86_64">x86_64</option>
                </select>
              </label>
              <label>
                <span>CPUs</span>
                <input type="number" min={1} max={32} value={form.cpus} onChange={(event) => setForm({ ...form, cpus: Number(event.target.value) })} />
              </label>
              <label>
                <span>Memory (MiB)</span>
                <input type="number" min={512} step={512} value={form.memoryMiB} onChange={(event) => setForm({ ...form, memoryMiB: Number(event.target.value) })} />
              </label>
              <label>
                <span>Disk (GiB)</span>
                <input type="number" min={5} value={form.diskGiB} onChange={(event) => setForm({ ...form, diskGiB: Number(event.target.value) })} />
              </label>
              <label className="toggleRow">
                <span><strong>Start after creation</strong></span>
                <input type="checkbox" checked={form.start} onChange={(event) => setForm({ ...form, start: event.target.checked })} />
              </label>
              <button type="submit" disabled={submitting || form.name.trim() === '' || form.image === ''}>{submitting ? 'Creating…' : 'Create machine'}</button>
            </form>
          </Inspector>
        )}
      </div>
    </>
  )
}
