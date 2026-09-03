import { useState } from 'react'
import { apiGet } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { StatusLamp } from '../components/StatusLamp'
import type { ActivityLevel, ActivityResourceSnapshot, ResourceUsage } from '../types'

const LEVEL_LAMP: Record<ActivityLevel, 'running' | 'starting' | 'crashed'> = {
  info: 'starting',
  notice: 'running',
  error: 'crashed',
}

function formatCPU(millicores: number) {
  if (millicores < 1000) return `${millicores}m`
  return `${(millicores / 1000).toFixed(millicores % 1000 === 0 ? 0 : 2)} cores`
}

function formatMemory(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let value = bytes
  let unit = -1
  do {
    value /= 1024
    unit += 1
  } while (value >= 1024 && unit < units.length - 1)
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[unit]}`
}

function usageLabel(usage: ResourceUsage) {
  return `${formatCPU(usage.cpuMillicores)} · ${formatMemory(usage.memoryBytes)}`
}

export function Activity() {
  const { entries, clearActivity } = useMessages()
  const [levelFilter, setLevelFilter] = useState<'all' | ActivityLevel>('all')
  const resources = usePolledResource<ActivityResourceSnapshot>(
    (signal) => apiGet('/api/activity/resources', signal),
    5000,
    [],
    'activity:resources',
  )
  const snapshot = resources.data
  const filtered = entries.filter((entry) => levelFilter === 'all' || entry.level === levelFilter)
  const counts = entries.reduce(
    (totals, entry) => { totals[entry.level] += 1; return totals },
    { info: 0, notice: 0, error: 0 } as Record<ActivityLevel, number>,
  )

  return (
    <>
      <section className="fleetRail" aria-label="Activity status">
        <span className="fleetRailTitle">Recent activity</span>
        <span className="fleetDatum"><StatusLamp state="starting" />Events <strong>{counts.info}</strong></span>
        <span className="fleetDatum"><StatusLamp state="running" />Notices <strong>{counts.notice}</strong></span>
        <span className="fleetDatum"><StatusLamp state="crashed" />Errors <strong>{counts.error}</strong></span>
        {snapshot && <span className="fleetDatum"><small>Total CPU</small><strong>{formatCPU(snapshot.total.cpuMillicores)}</strong></span>}
        {snapshot && <span className="fleetDatum"><small>Total memory</small><strong>{formatMemory(snapshot.total.memoryBytes)}</strong></span>}
        <span className="fleetMessage">{snapshot ? `${snapshot.partial ? 'partial · ' : ''}sampled ${new Date(snapshot.collectedAt).toLocaleTimeString([], { hour12: false })}` : `${entries.length} saved locally`}</span>
      </section>
      <div className="controlBar">
        <div className="statusFilters activityFilters" role="group" aria-label="Filter activity by level">
          {(['all', 'info', 'notice', 'error'] as const).map((level) => (
            <button
              type="button"
              className={`activityFilter-${level}${levelFilter === level ? ' active' : ''}`}
              aria-pressed={levelFilter === level}
              key={level}
              onClick={() => setLevelFilter(level)}
            >
              <span>{level}</span>
              <strong>{level === 'all' ? entries.length : counts[level]}</strong>
            </button>
          ))}
        </div>
        <span className="filterResultCount" aria-live="polite">{filtered.length} / {entries.length} entries</span>
        <button className="refreshControl destructiveAction" type="button" disabled={entries.length === 0} onClick={clearActivity}>Clear activity</button>
      </div>
      <div className="workArea activityWorkArea">
        <section className="activityResources" aria-label="Current resource consumption">
          <div className="activityResourcesHeading">
            <div>
              <span className="eyebrow">Live consumption</span>
              <h2>CPU and memory</h2>
            </div>
            <button className="refreshControl" type="button" onClick={resources.reload}>Refresh usage</button>
          </div>
          {resources.error && <p className="inlineError">{resources.error}</p>}
          {!snapshot && !resources.error && <p className="emptyDetail">Collecting current runtime usage…</p>}
          {snapshot && (
            <div className="activityResourceGroups">
              {snapshot.groups.map((group) => (
                <article className="activityResourceGroup" key={group.id}>
                  <header>
                    <div><strong>{group.name}</strong><small>{group.items.length} running item(s)</small></div>
                    <span>{usageLabel(group.total)}</span>
                  </header>
                  {group.error && <p className="inlineError">{group.error}</p>}
                  {group.items.length > 0 && (
                    <dl>
                      {group.items.map((item) => (
                        <div key={item.id}>
                          <dt>{item.name}<small>{item.detail || (item.countedInTotal ? 'included in total' : 'detail only')}</small></dt>
                          <dd>{usageLabel(item.usage)}</dd>
                        </div>
                      ))}
                    </dl>
                  )}
                </article>
              ))}
            </div>
          )}
        </section>
        <section className="channelBoard" aria-label="Client activity log">
          {filtered.length === 0 && (
            <article className="empty">
              <h2>{entries.length === 0 ? 'No activity recorded' : `No ${levelFilter} activity`}</h2>
              <p>{entries.length === 0 ? 'Actions, runtime events, and errors will appear here and persist across reloads.' : 'Choose another filter to see recorded activity.'}</p>
            </article>
          )}
          {filtered.length > 0 && (
            <div className="logViewport activityViewport">
              {filtered.map((entry) => (
                <div className={`logLine ${entry.level === 'error' ? 'stderr' : 'stdout'}`} key={entry.id}>
                  <time dateTime={entry.at}>{new Date(entry.at).toLocaleTimeString([], { hour12: false })}</time>
                  <span className="streamLabel"><StatusLamp state={LEVEL_LAMP[entry.level]} /> {entry.source}</span>
                  <span className="logMessage">{entry.message}</span>
                </div>
              ))}
            </div>
          )}
        </section>
      </div>
    </>
  )
}
