import { useRef, useState } from 'react'
import { apiDownload, apiGet, apiSend, apiUpload, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { DockerImage } from '../types'
import type { AnalysisDatabase, ImageAnalysisReport } from '../imageAnalysisTypes'

export function ImageAnalysis({ image, reference }: { image: DockerImage; reference: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const [platform, setPlatform] = useState(image.platform ?? '')
  const [reportID, setReportID] = useState('')
  const [severity, setSeverity] = useState('')
  const [fixable, setFixable] = useState(false)
  const [allowStale, setAllowStale] = useState(false)
  const [maxAgeHours, setMaxAgeHours] = useState(120)
  const [busy, setBusy] = useState(false)
  const databaseFile = useRef<HTMLInputElement>(null)
  const history = usePolledResource<ImageAnalysisReport[]>((signal) => apiGet('/api/docker/analysis', signal), 3000, [], 'image-analysis')
  const database = usePolledResource<AnalysisDatabase>((signal) => apiGet('/api/docker/analysis/database', signal), 60000, [], 'image-analysis:database')
  const matching = (history.data ?? []).filter((entry) => entry.image.digest === image.digest)
  const selectedID = reportID || matching[0]?.id || ''
  const detail = usePolledResource<ImageAnalysisReport | null>((signal) => selectedID ? apiGet(`/api/docker/analysis/${encodeURIComponent(selectedID)}`, signal) : Promise.resolve(null), 3000, [selectedID])
  const report = detail.data
  const running = report?.status === 'queued' || report?.status === 'running'
  const findings = (report?.findings ?? []).filter((finding) => (!severity || finding.severity.toLowerCase() === severity) && (!fixable || finding.fixState === 'fixed' && finding.fixedVersions.length > 0))

  async function run(action: () => Promise<void>) {
    setBusy(true)
    try { await action() } catch (error) { notifyError('image-analysis', errorMessage(error, 'Image analysis failed')) } finally { setBusy(false) }
  }

  function scan(force: boolean) {
    void run(async () => {
      const result = await apiSend<ImageAnalysisReport>(`/api/docker/images/${encodeURIComponent(reference)}/analysis`, 'POST', { platform, allowStale, maxAgeHours, force })
      setReportID(result.id)
      history.reload()
      detail.reload()
    })
  }

  function updateDatabase() {
    if (!window.confirm('Download the current Grype database from grype.anchore.io? Maximum 512 MiB and five minutes. No image contents are uploaded. Existing scans finish before database replacement.')) return
    void run(async () => {
      await apiSend('/api/docker/analysis/database/update', 'POST', { confirmNetwork: true, maxBytes: 512 * 1024 * 1024 })
      database.reload()
      notifyNotice('image-analysis', 'Grype database updated. Rescan images to use it.')
    })
  }

  async function exportReport(format: string) {
    if (!report) return
    await run(async () => {
      const blob = await apiDownload(`/api/docker/analysis/${encodeURIComponent(report.id)}/export?format=${format}`)
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `porto-${report.image.platform.replaceAll('/', '-')}-${format}.json`
      link.click()
      setTimeout(() => URL.revokeObjectURL(url), 1000)
    })
  }

  return <section className="drawerPanel">
    <h3>Local SBOM and vulnerabilities</h3>
    <p>Syft inventories local image bytes. Grype matches packages against a local advisory database. Dive only inspects layers.</p>
    <dl className="runtimeGrid">
      <div><dt>Database</dt><dd>{database.error || database.data?.state || 'Checking local database'}</dd></div>
      <div><dt>Built / maximum age</dt><dd>{database.data?.built || 'Not available'} / {database.data?.maxAgeHours ?? 120} hours</dd></div>
      <div><dt>Local storage</dt><dd className="mono">{database.data?.location || 'Unavailable'}</dd></div>
      <div><dt>Database size</dt><dd>{database.data ? `${(database.data.bytes / 1024 / 1024).toFixed(1)} MiB` : 'Unavailable'}</dd></div>
    </dl>
    {database.data?.message && <p role="status">{database.data.message}</p>}
    <div className="actions">
      <button type="button" disabled={busy} onClick={updateDatabase}>Download database</button>
      <button type="button" disabled={busy} onClick={() => databaseFile.current?.click()}>Import offline database</button>
      <button type="button" disabled={busy || running} onClick={() => {
        if (!window.confirm('Clear every saved image analysis and SBOM? Export needed reports first. The advisory database and images are preserved.')) return
        void run(async () => {
          await apiSend('/api/docker/analysis', 'DELETE')
          setReportID('')
          history.update(() => [])
          detail.update(() => null)
          history.reload()
        })
      }}>Clear saved analyses</button>
      <input ref={databaseFile} className="visuallyHidden" type="file" accept=".zst,.gz,.tar" aria-label="Offline Grype database archive" onChange={(event) => {
        const file = event.target.files?.[0]
        event.target.value = ''
        if (!file) return
        if (file.size > 512 * 1024 * 1024) { notifyError('image-analysis', 'Database archive exceeds 512 MiB.'); return }
        void run(async () => {
          await apiUpload('/api/docker/analysis/database/import', file)
          database.reload()
          notifyNotice('image-analysis', 'Offline database imported. Rescan images to use it.')
        })
      }} />
    </div>
    <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); scan(false) }}>
      <label>Selected local platform<input placeholder="Use the local image platform" value={platform} onChange={(event) => setPlatform(event.target.value)} disabled={busy || running} /></label>
      <label>Maximum database age (hours)<input type="number" min="1" max="8760" value={maxAgeHours} onChange={(event) => setMaxAgeHours(Number(event.target.value))} disabled={busy || running} required /></label>
      <label><input type="checkbox" checked={allowStale} onChange={(event) => setAllowStale(event.target.checked)} />Allow stale database matching (results remain stale)</label>
      <div className="actions">
        <button type="submit" disabled={busy || running}>Analyze image</button>
        <button type="button" disabled={busy || running} onClick={() => scan(true)}>Force rescan</button>
        {running && <button type="button" disabled={busy} onClick={() => void run(async () => {
          await apiSend(`/api/docker/analysis/${encodeURIComponent(selectedID)}/cancel`, 'POST')
          detail.reload()
        })}>Cancel analysis</button>}
      </div>
    </form>
    {history.error && <p role="alert">{history.error}</p>}
    {matching.length > 0 && <label className="inspectorForm">Recorded analyses<select value={selectedID} onChange={(event) => setReportID(event.target.value)}>
      {matching.map((entry) => <option key={entry.id} value={entry.id}>{entry.startedAt} / {entry.image.platform} / {entry.status}</option>)}
    </select></label>}
    {detail.error && <p role="alert">{detail.error}</p>}
    {!report && !detail.error && <p>No analysis selected. An unscanned image has unknown vulnerability status.</p>}
    {report && <>
      <p role="status"><strong>{report.status}</strong>: {report.phase}{report.cached ? ' (cached)' : ''}</p>
      {report.status !== 'succeeded' && <p>Incomplete, stale or failed analysis is not evidence of zero vulnerabilities.</p>}
      {report.error && <p role="alert">{report.error}</p>}
      {report.warnings?.map((warning, index) => <p role="status" key={index}>{warning}</p>)}
      <dl className="runtimeGrid">
        <div><dt>Manifest / platform</dt><dd className="mono">{report.image.manifestDigest || 'Not verified'} / {report.image.platform}</dd></div>
        <div><dt>Tools</dt><dd>Syft {report.syftVersion || 'unavailable'} / Grype {report.grypeVersion || 'unavailable'}</dd></div>
        <div><dt>Matched database</dt><dd>{report.database.built || 'Unavailable'} / {report.database.schemaVersion || 'unknown schema'}</dd></div>
        <div><dt>Distribution</dt><dd>{report.distribution || 'Unknown'}</dd></div>
        <div><dt>Base image</dt><dd>{report.baseImage}</dd></div>
        <div><dt>Verified layers</dt><dd>{report.layers?.length ?? 'Unavailable'}</dd></div>
      </dl>
      <div className="actions">
        <button type="button" disabled={busy} onClick={() => void exportReport('report')}>Export report JSON</button>
        {report.sbomId && <>
          <button type="button" disabled={busy} onClick={() => void exportReport('spdx-json')}>Export SPDX</button>
          <button type="button" disabled={busy} onClick={() => void exportReport('cyclonedx-json')}>Export CycloneDX</button>
        </>}
      </div>
      {report.findings && <>
        <h4>Advisory matches ({report.findings.length})</h4>
        <div className="inspectorForm inline">
          <label>Severity<select value={severity} onChange={(event) => setSeverity(event.target.value)}>
            <option value="">All severities</option>
            {['critical', 'high', 'medium', 'low', 'negligible', 'unknown'].map((value) => <option key={value} value={value}>{value}</option>)}
          </select></label>
          <label><input type="checkbox" checked={fixable} onChange={(event) => setFixable(event.target.checked)} />Recorded fixes only</label>
        </div>
        <div className="dataTableScroll"><table className="dataTable" aria-label="Grype vulnerability findings">
          <thead><tr><th>Severity</th><th>Advisory</th><th>Package</th><th>Fix and guidance</th></tr></thead>
          <tbody>{findings.map((finding, index) => <tr key={`${finding.id}:${finding.packageId}:${index}`}>
            <td>{finding.severity}</td>
            <td>{finding.advisory ? <a href={finding.advisory} target="_blank" rel="noreferrer">{finding.id}</a> : finding.id}<br />{finding.namespace}<br />{finding.matchTypes.join(', ')}</td>
            <td>{finding.package} {finding.version}</td>
            <td>{finding.fixState}: {finding.fixedVersions?.join(', ') || 'No recorded fixed version'}<br />{finding.remediation}</td>
          </tr>)}</tbody>
        </table></div>
        {!findings.length && <p>No matches in this filter. Coverage is determined by analysis status, not this count.</p>}
      </>}
      {report.packages && <details><summary>Packages, licenses and layer attribution ({report.packages.length})</summary>
        <div className="dataTableScroll"><table className="dataTable" aria-label="Local image SBOM packages">
          <thead><tr><th>Package</th><th>Type / PURL</th><th>Licenses</th><th>Source / layer</th></tr></thead>
          <tbody>{report.packages.map((pkg) => <tr key={pkg.id}><td>{pkg.name} {pkg.version}</td><td>{pkg.type}<br />{pkg.purl}</td><td>{pkg.licenses.join(', ') || 'Unknown'}</td><td>{pkg.locations.join(', ')}<br />{pkg.layers.join(', ') || 'Not attributed'}</td></tr>)}</tbody>
        </table></div>
      </details>}
    </>}
  </section>
}
