import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { StatusLamp } from '../components/StatusLamp'
import type {
  DiagnosticBundlePreview,
  DiagnosticCheck,
  DiagnosticReport,
  DiagnosticState,
  LampState,
} from '../types'

const STATES: DiagnosticState[] = ['healthy', 'degraded', 'unavailable', 'unsafe']
const STATE_LAMP: Record<DiagnosticState, LampState> = {
  healthy: 'running',
  degraded: 'starting',
  unavailable: 'stopped',
  unsafe: 'crashed',
}

function formatBytes(bytes: number) {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KiB', 'MiB', 'GiB']
  let value = bytes
  let unit = -1
  do {
    value /= 1024
    unit += 1
  } while (value >= 1024 && unit < units.length - 1)
  return `${value.toFixed(value >= 10 ? 1 : 2)} ${units[unit]}`
}

function repairKey(check: DiagnosticCheck) {
  return `${check.repair?.id ?? ''}:${check.repair?.target ?? ''}`
}

export function Diagnostics() {
  const { notifyError, notifyNotice, recordActivity } = useMessages()
  const [filter, setFilter] = useState<'all' | DiagnosticState>('all')
  const [preview, setPreview] = useState<DiagnosticBundlePreview | null>(null)
  const [previewBusy, setPreviewBusy] = useState(false)
  const [downloadBusy, setDownloadBusy] = useState(false)
  const [repairBusy, setRepairBusy] = useState('')
  const reportResource = usePolledResource<DiagnosticReport>(
    (signal) => apiGet('/api/diagnostics', signal),
    15000,
    [],
    'diagnostics:report',
  )
  const report = reportResource.data
  const filteredChecks = report?.checks.filter((check) => filter === 'all' || check.state === filter) ?? []

  async function loadPreview() {
    setPreviewBusy(true)
    try {
      const result = await apiGet<DiagnosticBundlePreview>('/api/diagnostics/bundle/preview')
      setPreview(result)
      recordActivity('info', 'diagnostics', `Diagnostic bundle preview contains ${result.entries.length} file(s).`)
    } catch (err) {
      notifyError('diagnostics', errorMessage(err, 'Unable to preview the diagnostic bundle'))
    } finally {
      setPreviewBusy(false)
    }
  }

  async function downloadBundle() {
    setDownloadBusy(true)
    try {
      const response = await fetch('/api/diagnostics/bundle')
      if (!response.ok) throw new Error((await response.text()) || `Bundle request failed with status ${response.status}`)
      const blob = await response.blob()
      const disposition = response.headers.get('content-disposition') ?? ''
      const filename = disposition.match(/filename="?([^"]+)"?/)?.[1] ?? 'porto-diagnostics.zip'
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = filename
      document.body.appendChild(anchor)
      anchor.click()
      anchor.remove()
      window.setTimeout(() => URL.revokeObjectURL(url), 0)
      notifyNotice('diagnostics', `Diagnostic bundle saved as ${filename}. Review it before sharing.`)
    } catch (err) {
      notifyError('diagnostics', errorMessage(err, 'Unable to download the diagnostic bundle'))
    } finally {
      setDownloadBusy(false)
    }
  }

  async function runRepair(check: DiagnosticCheck) {
    const repair = check.repair
    if (!repair || repairBusy !== '') return
    if (!window.confirm(`${repair.confirmation}\n\n${repair.description}`)) return
    const key = repairKey(check)
    setRepairBusy(key)
    try {
      await apiSend(`/api/diagnostics/repair/${repair.id}`, 'POST', {
        confirm: true,
        target: repair.target ?? '',
      })
      notifyNotice('diagnostics', `${repair.label} completed.`)
      reportResource.reload()
    } catch (err) {
      notifyError('diagnostics', errorMessage(err, `${repair.label} failed`))
    } finally {
      setRepairBusy('')
    }
  }

  return (
    <>
      <section className="fleetRail" aria-label="Diagnostic status">
        <span className="fleetRailTitle">Diagnostics</span>
        {STATES.map((state) => (
          <span className="fleetDatum" key={state}>
            <StatusLamp state={STATE_LAMP[state]} />{state}<strong>{report?.summary[state] ?? 0}</strong>
          </span>
        ))}
        <span className="fleetMessage" role="status" aria-live="polite">
          {report
            ? `${report.overall} · sampled ${new Date(report.generatedAt).toLocaleTimeString([], { hour12: false })}`
            : reportResource.loading ? 'running checks' : 'diagnostics unavailable'}
        </span>
      </section>
      <div className="controlBar">
        <div className="statusFilters diagnosticFilters" role="group" aria-label="Filter diagnostic checks">
          {(['all', ...STATES] as const).map((state) => (
            <button
              type="button"
              className={filter === state ? 'active' : ''}
              aria-pressed={filter === state}
              key={state}
              onClick={() => setFilter(state)}
            >
              <span>{state}</span>
              <strong>{state === 'all' ? report?.checks.length ?? 0 : report?.summary[state] ?? 0}</strong>
            </button>
          ))}
        </div>
        <span className="filterResultCount" aria-live="polite">{filteredChecks.length} check(s)</span>
        <button className="refreshControl" type="button" disabled={reportResource.loading} onClick={reportResource.reload}>Run checks</button>
        <button className="refreshControl" type="button" disabled={previewBusy} onClick={loadPreview}>{previewBusy ? 'Previewing…' : 'Preview bundle'}</button>
        <button className="refreshControl" type="button" disabled={downloadBusy} onClick={downloadBundle}>{downloadBusy ? 'Preparing…' : 'Download bundle'}</button>
      </div>
      <div className="workArea diagnosticsWorkArea">
        {reportResource.error && <p className="inlineError">{reportResource.error}</p>}
        {!report && !reportResource.error && <p className="emptyDetail">Running diagnostics…</p>}
        {report && (
          <section className="diagnosticChecks" aria-label="Diagnostic checks">
            {filteredChecks.length === 0 && (
              <article className="empty">
                <h2>No {filter} checks</h2>
                <p>Choose another status filter to inspect the current report.</p>
              </article>
            )}
            {filteredChecks.map((check) => (
              <article className={`diagnosticCheck diagnostic-${check.state}`} key={check.id}>
                <header>
                  <span><StatusLamp state={STATE_LAMP[check.state]} />{check.category}</span>
                  <strong>{check.name}</strong>
                  <em>{check.state}</em>
                </header>
                <p>{check.summary}</p>
                {check.detail && <pre>{check.detail}</pre>}
                {check.repair && (
                  <div className="diagnosticRepair">
                    <span>{check.repair.description}</span>
                    <button
                      type="button"
                      disabled={repairBusy !== ''}
                      onClick={() => runRepair(check)}
                    >
                      {repairBusy === repairKey(check) ? 'Repairing…' : check.repair.label}
                    </button>
                  </div>
                )}
              </article>
            ))}
          </section>
        )}
        {preview && (
          <section className="diagnosticBundle" aria-labelledby="diagnostic-bundle-title">
            <div className="activityResourcesHeading">
              <div>
                <span className="eyebrow">Local only</span>
                <h2 id="diagnostic-bundle-title">Bundle preview</h2>
              </div>
              <button type="button" className="refreshControl" onClick={() => setPreview(null)}>Close preview</button>
            </div>
            <p>Porto never uploads this bundle automatically. Review every file before sharing it.</p>
            <dl>
              {preview.entries.map((entry) => (
                <div key={entry.name}>
                  <dt>{entry.name}<small>{entry.description}</small></dt>
                  <dd>{formatBytes(entry.size)} · {entry.redactions} redaction(s)</dd>
                </div>
              ))}
            </dl>
            {preview.warnings?.map((warning, index) => <p className="inlineError" key={`${index}:${warning}`}>{warning}</p>)}
          </section>
        )}
      </div>
    </>
  )
}
