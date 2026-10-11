import { apiGet, apiSend, errorMessage } from '../api'
import { bytesLabel, type DataOperation } from '../dataTypes'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'

export function DataOperations({ resourceName }: { resourceName?: string }) {
  const { notifyError } = useMessages()
  const operations = usePolledResource<DataOperation[]>((signal) => apiGet('/api/data/operations', signal), 2000, [], 'data:operations')
  const rows = (operations.data ?? []).filter((run) => !resourceName || run.request.resource?.name === resourceName || run.request.destination === resourceName)
  async function cancel(id: number) {
    try { await apiSend(`/api/data/operations/${id}`, 'DELETE'); operations.reload() }
    catch (err) { notifyError('data', errorMessage(err, 'Unable to cancel data operation')) }
  }
  return <section className="drawerPanel">
    <h3>Data operation history</h3>
    {operations.error && <p role="alert" className="errorLine">{operations.error}</p>}
    <p className="hintLine">Jobs continue when this view closes. Cancel explicitly to stop a transfer; originals remain intact before a verified publish.</p>
    {!rows.length && <p>No data operations recorded.</p>}
    <div className="dataTableScroll"><table className="dataTable">
      <thead><tr><th>Operation / source</th><th>Progress / result</th><th>Controls</th></tr></thead>
      <tbody>{rows.map((run) => <tr key={run.id}>
        <td><strong>#{run.id} {run.request.action}</strong><br />{run.request.resource?.name || run.request.context}<br />{new Date(run.startedAt).toLocaleString()}</td>
        <td><span role={run.status === 'running' ? 'status' : undefined}>{run.status} · {run.phase} · {bytesLabel(run.bytes)}</span>
          {run.error && <p className="errorLine">{run.error}</p>}
          {run.result.message && <p>{run.result.message}</p>}
          {run.result.steps?.map((step, index) => <p key={index}>{step.kind}: {step.source} → {step.destination || 'removed'} · {step.status} {step.message}</p>)}
        </td>
        <td>{run.status === 'running' && <button type="button" onClick={() => void cancel(run.id)}>Cancel operation {run.id}</button>}
          {run.result.archive?.path && <a href={`/api/data/operations/${run.id}/archive`} download>Download verified archive</a>}
          {run.phase === 'archive-expired' && <span>Archive expired under retention</span>}
        </td>
      </tr>)}</tbody>
    </table></div>
  </section>
}
