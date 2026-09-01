import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ActionButton } from '../components/ActionButton'
import { Inspector } from '../components/Inspector'
import { InventoryList } from '../components/InventoryList'
import { StatusLamp } from '../components/StatusLamp'
import { lampStateFor } from '../components/lampState'
import { RuntimeGate } from '../components/SectionChrome'
import type { DockerContainer, DockerContainerAction, DockerStatus } from '../types'

const COLUMNS_TEMPLATE = '12px minmax(180px,1.3fr) minmax(160px,1fr) minmax(150px,1fr) minmax(120px,0.8fr)'

export function Containers() {
  const { notifyError, notifyNotice } = useMessages()
  const [query, setQuery] = useState('')
  const [selectedID, setSelectedID] = useState<string | null>(null)

  const status = usePolledResource<DockerStatus>((signal) => apiGet('/api/docker/status', signal), 10000, [], 'docker:status')
  const containers = usePolledResource<DockerContainer[]>(
    (signal) => apiGet('/api/docker/containers', signal),
    5000,
    [],
    'docker:containers',
  )
  const items = containers.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((container) => normalizedQuery === '' || [container.name, container.image, container.status]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const selected = items.find((container) => container.id === selectedID) ?? null
  const available = status.data?.available ?? false

  async function containerAction(container: DockerContainer, action: DockerContainerAction, confirmMessage?: string) {
    if (confirmMessage && !window.confirm(confirmMessage)) return
    try {
      await apiSend(`/api/docker/containers/${container.id}/${action}`, 'POST')
      notifyNotice('containers', `${action} succeeded for ${container.name}.`)
      containers.reload()
      if (action.startsWith('remove') && selectedID === container.id) setSelectedID(null)
    } catch (err) {
      notifyError('containers', errorMessage(err, `Unable to ${action} ${container.name}`))
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Docker status">
        <span className="fleetRailTitle">Docker signal</span>
        <span className="fleetDatum">
          <StatusLamp state={available ? 'running' : 'crashed'} />
          {available ? 'Available' : 'Unavailable'}
        </span>
        {status.data?.context && <span className="fleetDatum"><small>Context</small><strong>{status.data.context}</strong></span>}
        {status.data?.serverVersion && <span className="fleetDatum"><small>Engine</small><strong>{status.data.serverVersion}</strong></span>}
        <span className="fleetMessage">{items.length} container(s)</span>
      </section>
      <div className="controlBar">
        <label className="projectSearch">
          <span className="visuallyHidden">Filter containers</span>
          <svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="6.5" /><path d="m16 16 4 4" /></svg>
          <input type="search" value={query} placeholder="Filter containers by name, image, or status" onChange={(event) => setQuery(event.target.value)} />
        </label>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {items.length} containers</span>
        <button className="refreshControl" type="button" onClick={containers.reload}>Refresh</button>
      </div>
      <div className="workArea">
        {!available ? (
          <RuntimeGate
            label="Docker"
            enabled={status.data?.enabled ?? false}
            message={status.data?.message || status.error || (status.data?.enabled ? 'Confirm the configured Docker-compatible engine is running.' : undefined)}
          />
        ) : (
          <InventoryList
            items={filtered}
            getKey={(container) => container.id}
            columnsTemplate={COLUMNS_TEMPLATE}
            getLamp={(container) => lampStateFor(container.state)}
            getLampLabel={(container) => container.state}
            selectedKey={selectedID}
            onSelect={(container) => setSelectedID(container.id)}
            ariaLabel="Docker containers"
            emptyMessage={containers.error || 'No containers found.'}
            columns={[
              { header: 'Name', render: (container) => <strong>{container.name.replace(/^\//, '')}</strong> },
              { header: 'Image', className: 'mono', render: (container) => container.image },
              { header: 'Status', className: 'mono', render: (container) => container.status },
              { header: 'Ports', className: 'mono', render: (container) => container.ports || '—' },
            ]}
            renderActions={(container) => (
              <>
                <ActionButton label="Start container" icon="play" onClick={() => containerAction(container, 'start')} />
                <ActionButton label="Stop container" icon="stop" onClick={() => containerAction(container, 'stop')} />
                <ActionButton label="Restart container" icon="restart" onClick={() => containerAction(container, 'restart')} />
              </>
            )}
          />
        )}

        {selected && (
          <Inspector title={selected.name.replace(/^\//, '')} subtitle={selected.image} onClose={() => setSelectedID(null)}>
            <div className="drawerReadouts" aria-label="Container readouts">
              <span><small>State</small><strong>{selected.state}</strong></span>
              <span><small>Created</small><strong>{selected.createdAt || '—'}</strong></span>
            </div>
            <section className="drawerPanel">
              <h3>Runtime detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Status</dt><dd>{selected.status}</dd></div>
                <div><dt>Ports</dt><dd>{selected.ports || '—'}</dd></div>
                <div><dt>Networks</dt><dd>{selected.networks || '—'}</dd></div>
                <div><dt>Mounts</dt><dd>{selected.mounts || '—'}</dd></div>
              </dl>
            </section>
            <div className="maintenanceBar">
              <span>Maintenance controls</span>
              <div className="actions">
                <ActionButton label="Pause container" icon="pause" onClick={() => containerAction(selected, 'pause')} />
                <ActionButton label="Unpause container" icon="play" onClick={() => containerAction(selected, 'unpause')} />
                <ActionButton
                  className="removeButton"
                  label="Remove container"
                  icon="remove"
                  onClick={() => containerAction(selected, 'remove', `Remove container ${selected.name}?`)}
                />
                <ActionButton
                  className="removeButton"
                  label="Force remove container"
                  icon="kill"
                  onClick={() => containerAction(selected, 'remove-force', `Force-remove container ${selected.name} even if running?`)}
                />
              </div>
            </div>
          </Inspector>
        )}
      </div>
    </>
  )
}
