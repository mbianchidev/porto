import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { useContainerSnapshots } from '../containerSnapshots'
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
  const containers = useContainerSnapshots(status.data?.enabled ?? false)
  const items = containers.data ?? []
  const normalizedQuery = query.trim().toLocaleLowerCase()
  const filtered = items.filter((container) => normalizedQuery === '' || [container.name, container.image, container.status]
    .some((value) => value.toLocaleLowerCase().includes(normalizedQuery)))
  const selected = items.find((container) => container.id === selectedID) ?? null
  const available = containers.snapshot?.available ?? status.data?.available ?? false
  const stale = containers.snapshot?.stale ?? status.data?.stale ?? false

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
          <StatusLamp state={available ? 'running' : stale ? 'starting' : 'crashed'} />
          {available ? 'Live' : stale ? 'Stale' : 'Unavailable'}
        </span>
        {status.data?.context && <span className="fleetDatum"><small>Context</small><strong>{status.data.context}</strong></span>}
        {containers.snapshot?.namespace && <span className="fleetDatum"><small>Namespace</small><strong>{containers.snapshot.namespace}</strong></span>}
        <span className="fleetMessage">
          {items.length} container(s) · revision {containers.snapshot?.revision ?? 0}
          {!containers.connected && available ? ' · reconnecting updates' : ''}
        </span>
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
            message={containers.snapshot?.message || containers.error || status.data?.message || status.error || (status.data?.enabled ? 'Confirm the configured Docker-compatible engine is running.' : undefined)}
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
              <span><small>Task</small><strong>{selected.taskPresent ? `PID ${selected.pid ?? '—'}` : 'No task'}</strong></span>
              <span><small>Health</small><strong>{selected.health.status}</strong></span>
              <span><small>Restarts</small><strong>{selected.restartCount}</strong></span>
              <span><small>Created</small><strong>{selected.createdAt || '—'}</strong></span>
            </div>
            <section className="drawerPanel">
              <h3>Runtime detail</h3>
              <dl className="runtimeGrid">
                <div><dt>Status</dt><dd>{selected.status}</dd></div>
                <div><dt>Ports</dt><dd>{selected.ports || '—'}</dd></div>
                <div><dt>Networks</dt><dd>{selected.networks || '—'}</dd></div>
                <div><dt>Mounts</dt><dd>{selected.mounts || '—'}</dd></div>
                <div><dt>Exit</dt><dd>{selected.exitCode === undefined ? '—' : `${selected.exitCode}${selected.exitSignal ? ` (signal ${selected.exitSignal})` : ''}`}</dd></div>
                <div><dt>Exit reason</dt><dd>{selected.exitReason || '—'}</dd></div>
                <div><dt>Restart policy</dt><dd>{selected.restartPolicy || 'none'}</dd></div>
                <div><dt>CPU quota</dt><dd>{selected.resources.cpuQuota ?? '—'}</dd></div>
                <div><dt>Memory limit</dt><dd>{selected.resources.memoryLimit ?? '—'}</dd></div>
                <div><dt>Stop behavior</dt><dd>{selected.stopSignal || 'default'} / {selected.stopTimeout ?? 0}s</dd></div>
              </dl>
              {selected.inventoryError && <p className="errorText">{selected.inventoryError}</p>}
            </section>
            {selected.history && selected.history.length > 0 && (
              <section className="drawerPanel">
                <h3>Lifecycle history</h3>
                <dl className="runtimeGrid">
                  {selected.history.slice(-6).reverse().map((event) => (
                    <div key={event.sequence}>
                      <dt>{event.type}</dt>
                      <dd>{event.reason || event.topic} · {event.timestamp}</dd>
                    </div>
                  ))}
                </dl>
              </section>
            )}
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
