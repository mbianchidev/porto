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
import type { DockerStatus, DockerVolume } from '../types'

const COLUMNS_TEMPLATE = 'minmax(180px,1.3fr) minmax(100px,0.6fr) minmax(220px,1.4fr) minmax(90px,0.5fr)'

export function Volumes() {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedName, setSelectedName] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [driver, setDriver] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [])
  const volumes = usePolledResource<DockerVolume[]>((signal) => apiGet('/api/docker/volumes', signal), 8000, [])
  const items = volumes.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((volume) => normalizedQuery === '' || volume.name.toLocaleLowerCase().includes(normalizedQuery))
  const selected = items.find((volume) => volume.name === selectedName) ?? null
  const available = status.data?.available ?? false

  async function createVolume(event: FormEvent) {
    event.preventDefault()
    if (name.trim() === '') return
    setSubmitting(true)
    try {
      await apiSend('/api/docker/volumes', 'POST', { name: name.trim(), driver: driver.trim() })
      notifyNotice('volumes', `Created volume ${name.trim()}.`)
      setName('')
      setDriver('')
      setCreating(false)
      volumes.reload()
    } catch (err) {
      notifyError('volumes', errorMessage(err, `Unable to create volume ${name}`))
    } finally {
      setSubmitting(false)
    }
  }

  async function removeVolume(volume: DockerVolume, force: boolean) {
    if (!window.confirm(`${force ? 'Force-remove' : 'Remove'} volume ${volume.name}?`)) return
    try {
      await apiSend(`/api/docker/volumes/${volume.name}${force ? '?force=true' : ''}`, 'DELETE')
      notifyNotice('volumes', `Removed volume ${volume.name}.`)
      if (selectedName === volume.name) setSelectedName(null)
      volumes.reload()
    } catch (err) {
      notifyError('volumes', errorMessage(err, `Unable to remove volume ${volume.name}`))
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Volume signal</span>
        <span className="fleetDatum"><StatusLamp state={available ? 'running' : 'crashed'} />{available ? 'Available' : 'Unavailable'}</span>
        <span className="fleetMessage">{items.length} volume(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter volumes</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter volumes by name" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} volumes</span>
        <button className="refreshControl" type="button" onClick={volumes.reload}>Refresh</button>
        <button type="button" disabled={!available} onClick={() => { setSelectedName(null); setCreating(true) }}>New volume</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate label="Docker" enabled={status.data?.enabled ?? false} message={status.data?.message || status.error} />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(volume) => volume.name}
            columnsTemplate={COLUMNS_TEMPLATE}
            selectedKey={selectedName}
            onSelect={(volume) => { setCreating(false); setSelectedName(volume.name) }}
            ariaLabel="Docker volumes"
            emptyMessage={volumes.error || 'No volumes found.'}
            columns={[
              { header: 'Name', render: (volume) => <strong>{volume.name}</strong> },
              { header: 'Driver', className: 'mono', render: (volume) => volume.driver },
              { header: 'Mountpoint', className: 'mono', render: (volume) => volume.mountpoint },
              { header: 'Scope', className: 'mono', render: (volume) => volume.scope },
            ]}
            renderActions={(volume) => (
              <ActionButton className="removeButton" label="Remove volume" icon="remove" onClick={() => removeVolume(volume, false)} />
            )}
          />
        )}

        {selected && !creating && (
          <Inspector title={selected.name} subtitle={selected.driver} onClose={() => setSelectedName(null)}>
            <section className="drawerPanel">
              <h3>Volume detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Mountpoint</dt><dd>{selected.mountpoint}</dd></div>
                <div><dt>Scope</dt><dd>{selected.scope}</dd></div>
                <div><dt>Created</dt><dd>{selected.createdAt || '—'}</dd></div>
              </dl>
            </section>
            <div className="maintenanceBar">
              <span>Maintenance controls</span>
              <div className="actions">
                <ActionButton className="removeButton" label="Remove volume" icon="remove" onClick={() => removeVolume(selected, false)} />
                <ActionButton className="removeButton" label="Force remove volume" icon="kill" onClick={() => removeVolume(selected, true)} />
              </div>
            </div>
          </Inspector>
        )}

        {creating && (
          <Inspector title="New volume" subtitle="docker volume create" onClose={() => setCreating(false)}>
            <form className="inspectorForm" onSubmit={createVolume}>
              <label>
                <span>Name</span>
                <input type="text" value={name} placeholder="app-data" onChange={(event) => setName(event.target.value)} required />
              </label>
              <label>
                <span>Driver</span>
                <input type="text" value={driver} placeholder="local" onChange={(event) => setDriver(event.target.value)} />
              </label>
              <button type="submit" disabled={submitting || name.trim() === ''}>{submitting ? 'Creating…' : 'Create volume'}</button>
            </form>
          </Inspector>
        )}
      </div>
    </>
  )
}
