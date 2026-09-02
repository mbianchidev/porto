import { useState } from 'react'
import { useMessages } from '../useMessages'
import { StatusLamp } from '../components/StatusLamp'
import type { ActivityLevel } from '../types'

const LEVEL_LAMP: Record<ActivityLevel, 'running' | 'starting' | 'crashed'> = {
  info: 'starting',
  notice: 'running',
  error: 'crashed',
}

export function Activity() {
  const { entries, clearActivity } = useMessages()
  const [levelFilter, setLevelFilter] = useState<'all' | ActivityLevel>('all')
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
        <span className="fleetMessage">{entries.length} saved locally</span>
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
