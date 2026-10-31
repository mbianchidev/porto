import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { getEnvironmentID } from '../environment'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { InstalledExtension } from '../managementTypes'

export function ExtensionTools({ installation, resourceKind = '', resourceID = '' }: { installation: InstalledExtension; resourceKind?: string; resourceID?: string }) {
  const { notifyError } = useMessages()
  const [args, setArgs] = useState('{}')
  const [result, setResult] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)
  const views = installation.manifest.views.filter((view) => resourceKind ? view.location === 'resource' && view.resourceKind === resourceKind : view.location === 'tools')
  const actions = installation.manifest.actions.filter((action) => resourceKind ? action.resourceKind === resourceKind : !action.resourceKind)
  if (!installation.enabled || (!views.length && !actions.length)) return null
  const risky = installation.grants.permissions.some((permission) => ['runtime.control', 'host.files.write', 'process.exec'].includes(permission))
  return <section className="drawerPanel" aria-label={`${installation.name} contributions`}>
    <h3>{installation.name}</h3>
    <p>{installation.manifest.description} Target: <span className="mono">{getEnvironmentID()}{resourceID ? ` / ${resourceID}` : ''}</span>.</p>
    {views.map((view) => <iframe key={`${view.id}:${installation.generation}`} className="extensionFrame" title={`${installation.name}: ${view.title}`} sandbox="" referrerPolicy="no-referrer" src={`/api/extensions/${encodeURIComponent(installation.id)}/ui?generation=${installation.generation}`} />)}
    {actions.length > 0 && <><label>Action arguments (JSON)<textarea value={args} onChange={(event) => setArgs(event.target.value)} /></label><div className="actions">
      {actions.map((action) => <button key={action.id} type="button" disabled={busy} onClick={async () => {
        const environmentId = getEnvironmentID()
        const confirm = action.destructive || risky
        if (confirm && !window.confirm(`Run ${action.title} from ${installation.name} against ${environmentId}${resourceID ? ` / ${resourceID}` : ''}? Approved runtime control, host-file writes or exact native processes may have lasting effects. Review this extension's grants before continuing.`)) return
        setBusy(true); setResult(null)
        try {
          const parsed: unknown = JSON.parse(args)
          const response = await apiSend<{ environmentId: string; data: unknown }>(`/api/extensions/${encodeURIComponent(installation.id)}/invocations`, 'POST', { action: action.id, environmentId, resourceKind, resourceId: resourceID, args: parsed, confirm })
          if (response.environmentId !== environmentId) throw new Error('Extension result target did not match the explicitly requested environment.')
          setResult(response.data)
        } catch (error) { notifyError('extensions', errorMessage(error, 'Extension action failed')) } finally { setBusy(false) }
      }}>{action.title}</button>)}
      {busy && <button type="button" onClick={async () => {
        try { await apiSend(`/api/extensions/${encodeURIComponent(installation.id)}/cancellation`, 'POST') }
        catch (error) { notifyError('extensions', errorMessage(error, 'Unable to cancel extension')) }
      }}>Cancel extension action</button>}
    </div></>}
    {result !== null && <pre className="definitionText" aria-label="Extension action result">{JSON.stringify(result, null, 2)}</pre>}
  </section>
}

export function ExtensionContributions({ resourceKind, resourceID }: { resourceKind: string; resourceID: string }) {
  const installed = usePolledResource<InstalledExtension[]>((signal) => apiGet('/api/extensions', signal), 5000, [])
  return <>{installed.error && <p role="alert">Extension contributions unavailable: {installed.error}</p>}{(installed.data ?? []).filter((item) => item.enabled).map((item) => <ExtensionTools key={`${item.id}:${item.generation}:${resourceID}`} installation={item} resourceKind={resourceKind} resourceID={resourceID} />)}</>
}
