import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { LocalOperation } from '../deliveryTypes'

export function LocalOperations({ resource }: { resource?: string }) {
  const { notifyError, notifyNotice } = useMessages()
  const operations = usePolledResource<LocalOperation[]>((signal) => apiGet('/api/operations', signal), 2000, [], 'local:operations')
  const values = (operations.data ?? []).filter((operation) => !resource || operation.resource === resource).slice(0, 30)
  return <section className="drawerPanel" aria-label="Local delivery and administration operations">
    <h3>Operation history</h3>
    {operations.error && <p role="alert">{operations.error}</p>}
    {!values.length && <p>No recorded local operations.</p>}
    {values.length > 0 && <div className="dataTableScroll"><table className="dataTable">
      <thead><tr><th>Action / resource</th><th>Status</th><th>Progress / failure</th><th>Started</th><th>Control</th></tr></thead>
      <tbody>{values.map((operation) => <tr key={operation.id}>
        <td>{operation.action}<br /><span className="mono">{operation.resource}</span></td>
        <td>{operation.status}</td><td>{operation.phase}{operation.error && <p role="status">{operation.error}</p>}</td><td>{operation.startedAt}</td>
        <td>{['queued', 'running'].includes(operation.status) && <button type="button" onClick={async () => {
          try {
            await apiSend(`/api/operations/${encodeURIComponent(operation.id)}/cancel`, 'POST')
            notifyNotice('operations', 'Cancellation requested. Inspect current state before retrying.')
            operations.reload()
          } catch (error) { notifyError('operations', errorMessage(error, 'Unable to cancel operation')) }
        }}>Cancel operation</button>}</td>
      </tr>)}</tbody>
    </table></div>}
  </section>
}
