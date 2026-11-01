import { setEnvironmentID, useEnvironmentID, SOURCE_HOST_URL_NOTICE } from '../environment'
import type { EnvironmentProfile } from '../managementTypes'
import { useMessages } from '../useMessages'
import { errorMessage } from '../api'

export function EnvironmentSelector({ profiles, error }: { profiles: EnvironmentProfile[]; error: string }) {
  const active = useEnvironmentID()
  const { notifyError } = useMessages()
  const selected = profiles.find((profile) => profile.id === active)
  return <section className="environmentBar" aria-label="Selected environment">
    <label>Environment<select value={active} onChange={(event) => {
      try { setEnvironmentID(event.target.value) } catch (error) { notifyError('environments', errorMessage(error, 'Unable to select environment')) }
    }}>
      {!selected && <option value={active}>{active === 'local' ? 'This machine' : `Unavailable target ${active}`}</option>}
      {profiles.map((profile) => <option key={profile.id} value={profile.id}>{profile.name} / {profile.backend} / {profile.state}</option>)}
    </select></label>
    <span className="mono">{active}</span>
    <span>{selected?.identity || (active === 'local' ? 'Local OS user' : 'Unknown identity')}</span>
    {active !== 'local' && <strong role="status">{selected?.revoked ? 'Revoked' : selected?.state || 'Unavailable'}{selected?.lastSeen ? ` / last contact ${new Date(selected.lastSeen).toLocaleTimeString()}` : ''}</strong>}
    <a href="#/environments">Manage targets</a>
    {error && <span role="alert">{error}</span>}
    {active !== 'local' && (!selected || selected.revoked || selected.state !== 'online') && <span className="environmentWarning">Inventory may be stale. Actions never fall back to this machine or replay automatically.</span>}
    {active !== 'local' && <span>{SOURCE_HOST_URL_NOTICE}</span>}
  </section>
}
