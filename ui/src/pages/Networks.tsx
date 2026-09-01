import { useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { RuntimeGate } from '../components/SectionChrome'
import type { DockerCreateNetworkRequest, DockerNetwork, DockerStatus } from '../types'

const COLUMNS_TEMPLATE = 'minmax(170px,1.2fr) minmax(100px,0.6fr) minmax(80px,0.4fr) minmax(80px,0.4fr) minmax(70px,0.4fr)'
const EMPTY_FORM: DockerCreateNetworkRequest = { name: '', driver: '', subnet: '', gateway: '', internal: false }

export function Networks() {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedName, setSelectedName] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<DockerCreateNetworkRequest>(EMPTY_FORM)
  const [submitting, setSubmitting] = useState(false)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const networks = usePolledResource<DockerNetwork[]>((signal) => apiGet('/api/docker/networks', signal), 8000, [], 'docker:networks')
  const items = networks.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((network) => normalizedQuery === '' || network.name.toLocaleLowerCase().includes(normalizedQuery))
  const selected = items.find((network) => network.name === selectedName) ?? null
  const available = status.data?.available ?? false

  async function createNetwork(event: FormEvent) {
    event.preventDefault()
    if (form.name.trim() === '') return
    setSubmitting(true)
    try {
      await apiSend('/api/docker/networks', 'POST', { ...form, name: form.name.trim() })
      notifyNotice('networks', `Created network ${form.name.trim()}.`)
      setForm(EMPTY_FORM)
      setCreating(false)
      networks.reload()
    } catch (err) {
      notifyError('networks', errorMessage(err, `Unable to create network ${form.name}`))
    } finally {
      setSubmitting(false)
    }
  }

  async function removeNetwork(network: DockerNetwork) {
    if (!window.confirm(`Remove network ${network.name}?`)) return
    try {
      await apiSend(`/api/docker/networks/${network.name}`, 'DELETE')
      notifyNotice('networks', `Removed network ${network.name}.`)
      if (selectedName === network.name) setSelectedName(null)
      networks.reload()
    } catch (err) {
      notifyError('networks', errorMessage(err, `Unable to remove network ${network.name}`))
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Network signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetMessage">{items.length} network(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter networks</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter networks by name" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} networks</span>
        <button className="refreshControl" type="button" onClick={networks.reload}>Refresh</button>
        <button type="button" disabled={!available} onClick={() => { setSelectedName(null); setCreating(true) }}>New network</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Docker" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(network) => network.id}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedName}
            onSelect={(network) => { setCreating(false); setSelectedName(network.name) }}
            ariaLabel="Docker networks"
            emptyMessage={networks.error || 'No networks found.'}
            columns={[
              { header: 'Name', render: (network) => <strong>{network.name}</strong> },
              { header: 'Driver', className: 'mono', render: (network) => network.driver },
              { header: 'Scope', className: 'mono', render: (network) => network.scope },
              { header: 'Internal', className: 'mono', render: (network) => network.internal },
              { header: 'IPv6', className: 'mono', render: (network) => network.ipv6 },
            ]}
            renderActions={(network) => (
              <ActionButton className="removeButton" label="Remove network" icon="remove" onClick={() => removeNetwork(network)} />
            )}
          />
        )}

        {selected && !creating && (
          <Inspector title={selected.name} subtitle={selected.driver} onClose={() => setSelectedName(null)}>
            <section className="drawerPanel">
              <h3>Network detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Scope</dt><dd>{selected.scope}</dd></div>
                <div><dt>Internal</dt><dd>{selected.internal}</dd></div>
                <div><dt>IPv6</dt><dd>{selected.ipv6}</dd></div>
                <div><dt>Created</dt><dd>{selected.createdAt || '—'}</dd></div>
              </dl>
            </section>
            <div className="maintenanceBar">
              <span>Maintenance controls</span>
              <div className="actions">
                <ActionButton className="removeButton" label="Remove network" icon="remove" onClick={() => removeNetwork(selected)} />
              </div>
            </div>
          </Inspector>
        )}

        {creating && (
          <Inspector title="New network" subtitle="docker network create" onClose={() => setCreating(false)}>
            <form className="inspectorForm" onSubmit={createNetwork}>
              <label>
                <span>Name</span>
                <input type="text" value={form.name} placeholder="app-net" onChange={(event) => setForm({ ...form, name: event.target.value })} required />
              </label>
              <label>
                <span>Driver</span>
                <input type="text" value={form.driver} placeholder="bridge" onChange={(event) => setForm({ ...form, driver: event.target.value })} />
              </label>
              <label>
                <span>Subnet</span>
                <input type="text" value={form.subnet} placeholder="172.30.0.0/16" onChange={(event) => setForm({ ...form, subnet: event.target.value })} />
              </label>
              <label>
                <span>Gateway</span>
                <input type="text" value={form.gateway} placeholder="172.30.0.1" onChange={(event) => setForm({ ...form, gateway: event.target.value })} />
              </label>
              <label className="toggleRow">
                <span><strong>Internal network</strong></span>
                <input type="checkbox" checked={form.internal} onChange={(event) => setForm({ ...form, internal: event.target.checked })} />
              </label>
              <button type="submit" disabled={submitting || form.name.trim() === ''}>{submitting ? 'Creating…' : 'Create network'}</button>
            </form>
          </Inspector>
        )}
      </div>
    </>
  )
}
