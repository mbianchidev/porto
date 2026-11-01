import { lazy, Suspense, useState } from 'react'
import { PodLogs } from '../pages/Pods'
import type { AdminObject } from '../kubeAdminTypes'

const Terminal = lazy(async () => ({ default: (await import('./VMTerminal')).InteractiveTerminal }))
function object(value: unknown): value is Record<string, unknown> { return Boolean(value) && typeof value === 'object' && !Array.isArray(value) }

export function KubernetesPodSessions({ resource, context, terminalAllowed }: { resource: AdminObject; context: string; terminalAllowed: boolean }) {
  const [logsOpen, setLogsOpen] = useState(false)
  const [terminalOpen, setTerminalOpen] = useState(false)
  if (resource.kind !== 'Pod' || !resource.target.uid) return null
  const spec = resource.manifest.spec
  const status = resource.manifest.status
  const rawContainers = object(spec) && Array.isArray(spec.containers) ? spec.containers : []
  const statuses = object(status) && Array.isArray(status.containerStatuses) ? status.containerStatuses : []
  const containers = rawContainers.flatMap((entry: unknown) => {
    if (!object(entry) || typeof entry.name !== 'string') return []
    const state: unknown = statuses.find((item: unknown) => object(item) && item.name === entry.name)
    return [{ name: entry.name, restartCount: object(state) && typeof state.restartCount === 'number' ? state.restartCount : 0 }]
  })
  const pod = { uid: resource.target.uid, namespace: resource.target.namespace, name: resource.target.name, containers }
  const running = object(status) && status.phase === 'Running'
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const query = new URLSearchParams({ context, uid: pod.uid, shell: 'sh' })
  const endpoint = `${protocol}//${window.location.host}/api/kubernetes/pods/${encodeURIComponent(pod.namespace)}/${encodeURIComponent(pod.name)}/terminal?${query}`
  return <section aria-label="Selected pod sessions">
    <div className="actions">
      <button type="button" onClick={() => setLogsOpen((value) => !value)}>{logsOpen ? 'Close pod logs' : 'Open pod logs'}</button>
      <button type="button" disabled={!terminalAllowed || !running} onClick={() => setTerminalOpen((value) => !value)}>{terminalOpen ? 'Close pod terminal' : 'Open pod terminal'}</button>
    </div>
    {logsOpen && <PodLogs pod={pod} context={context} />}
    {terminalOpen && <Suspense fallback={<p role="status">Loading selected pod terminal...</p>}><Terminal endpoint={endpoint} title="Pod terminal" detail={`${context} / ${pod.namespace}/${pod.name}`} running={running} ariaLabel={`Interactive terminal for ${pod.name}`} stoppedMessage="Select a running pod with an explicit terminal grant." /></Suspense>}
  </section>
}
