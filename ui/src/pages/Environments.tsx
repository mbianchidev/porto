import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { setEnvironmentID } from '../environment'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { EnvironmentProfile, FleetPreview, FleetRun } from '../managementTypes'

const CAPABILITIES = ['inventory.read', 'workload.operate', 'streams.read', 'terminal.open', 'infrastructure.operate', 'secrets.read']
type Listener = { address: string; hostname: string; publicAddress: string; certificate?: string }
type Identity = { id: string; name: string; kind: string; revoked: boolean; expiresAt: string; capabilities: string[] }

function labelsFrom(text: string): Record<string, string> {
  const value: unknown = JSON.parse(text)
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some((item) => typeof item !== 'string')) throw new Error('Labels must be a JSON object with string values.')
  return value as Record<string, string>
}

export function Environments() {
  const { notifyError, notifyNotice } = useMessages()
  const profiles = usePolledResource<EnvironmentProfile[]>((signal) => apiGet('/api/environments', signal), 5000, [])
  const listener = usePolledResource<Listener | null>((signal) => apiGet('/api/environments/listener', signal), 0, [])
  const identities = usePolledResource<Identity[]>((signal) => apiGet('/api/environments/identities', signal), 10000, [])
  const runs = usePolledResource<FleetRun[]>((signal) => apiGet('/api/environments/fleet/runs', signal), 3000, [])
  const [name, setName] = useState('')
  const [backend, setBackend] = useState('porto')
  const [address, setAddress] = useState('')
  const [certificate, setCertificate] = useState('')
  const [token, setToken] = useState('')
  const [clientCertificate, setClientCertificate] = useState('')
  const [clientKey, setClientKey] = useState('')
  const [kubeconfig, setKubeconfig] = useState('')
  const [context, setContext] = useState('')
  const [operate, setOperate] = useState(false)
  const [importTerminal, setImportTerminal] = useState(false)
  const [importSecrets, setImportSecrets] = useState(false)
  const [labels, setLabels] = useState('{}')
  const [filter, setFilter] = useState('')
  const [busy, setBusy] = useState(false)
  const [listen, setListen] = useState('127.0.0.1:37624')
  const [hostname, setHostname] = useState('127.0.0.1')
  const [publicAddress, setPublicAddress] = useState('https://127.0.0.1:37624')
  const [enrollmentName, setEnrollmentName] = useState('')
  const [kind, setKind] = useState('client')
  const [capabilities, setCapabilities] = useState(['inventory.read'])
  const [enrollment, setEnrollment] = useState('')
  const [targets, setTargets] = useState<Record<string, string>>({})
  const [action, setAction] = useState('stop')
  const [preview, setPreview] = useState<FleetPreview | null>(null)
  const [confirmation, setConfirmation] = useState('')
  const [metadata, setMetadata] = useState<{ id: string; name: string; labels: string; revision: number } | null>(null)

  async function run(work: () => Promise<void>) {
    if (busy) return
    setBusy(true)
    try { await work() } catch (error) { notifyError('environments', errorMessage(error, 'Environment operation failed')) } finally { setBusy(false) }
  }
  function refresh() { profiles.reload(); identities.reload(); listener.reload(); runs.reload() }
  function add() {
    void run(async () => {
      await apiSend('/api/environments', 'POST', {
        name, backend, address, certificate, labels: labelsFrom(labels), operate, context, kubeconfig,
        capabilities: backend === 'porto' ? [] : [...(importSecrets ? ['secrets.read'] : []), ...(backend === 'kubernetes' && importTerminal ? ['terminal.open'] : [])],
        ...(backend === 'porto' ? { enrollmentToken: token } : { token, clientCertificate, clientKey }),
      })
      setToken(''); setClientKey(''); setClientCertificate(''); setKubeconfig('')
      setName(''); setCertificate(''); refresh()
      notifyNotice('environments', 'Environment registered. Credentials are in the OS credential store; registering a cluster does not confer infrastructure ownership.')
    })
  }
  function selectTarget(profile: EnvironmentProfile) {
    try {
      setEnvironmentID(profile.id)
      window.location.assign(profile.backend === 'kubernetes' ? '#/kubernetes-admin' : '#/containers')
    } catch (error) { notifyError('environments', errorMessage(error, 'Unable to select target')) }
  }
  const rows = (profiles.data ?? []).filter((profile) => `${profile.name} ${profile.id} ${profile.backend} ${Object.entries(profile.labels ?? {}).flat().join(' ')}`.toLowerCase().includes(filter.toLowerCase()))

  return <>
    <section className="fleetRail" aria-label="Environment fleet status"><span className="fleetRailTitle">Environments and fleets</span><span className="fleetDatum">{profiles.data?.length ?? 0} targets</span><span className="fleetMessage">Explicit identities. No account required. Offline actions are never queued or replayed.</span></section>
    <div className="controlBar"><label>Filter names, groups or labels<input value={filter} onChange={(event) => setFilter(event.target.value)} /></label><button type="button" onClick={refresh}>Refresh environments</button></div>
    <div className="workArea managementArea">
      {profiles.error && <p role="alert">{profiles.error}</p>}
      <div className="dataTableScroll"><table className="dataTable"><caption>Authorized environments</caption><thead><tr><th>Target and identity</th><th>Connection and capabilities</th><th>Controls</th></tr></thead><tbody>
        {rows.map((profile) => <tr key={profile.id}>
          <td><strong>{profile.name}</strong><p className="mono">{profile.id}</p><p>{profile.backend} / {profile.address}</p><p>{profile.identity}</p><p>{Object.entries(profile.labels ?? {}).map(([key, value]) => `${key}=${value}`).join(', ')}</p><p>{profile.backend === 'kubernetes' ? 'Imported, infrastructure not owned' : profile.owned ? 'Local ownership' : 'Explicit remote authorization only'}</p></td>
          <td><strong>{profile.revoked ? 'revoked' : profile.state}</strong><p>{profile.lastSeen ? `Last contact: ${new Date(profile.lastSeen).toLocaleString()}` : 'No successful contact recorded'}</p><p>{profile.capabilities.join(', ')}</p>{profile.lastError && <p role="alert">{profile.lastError}</p>}{profile.unsupported?.map((message) => <p key={message}>{message}</p>)}</td>
          <td><div className="actions"><button type="button" disabled={profile.revoked} onClick={() => selectTarget(profile)}>Use {profile.name}</button>
            {profile.id !== 'local' && <>
              <button type="button" disabled={busy} onClick={() => setMetadata({ id: profile.id, name: profile.name, labels: JSON.stringify(profile.labels ?? {}, null, 2), revision: profile.revision })}>Edit labels for {profile.name}</button>
              <button type="button" disabled={busy || profile.revoked} onClick={() => void run(async () => {
                const path = profile.backend === 'kubernetes' ? `/api/kubernetes/status?context=${encodeURIComponent(profile.context ?? '')}` : '/api/docker/status'
                await apiGet(`/api/environments/${profile.id}${path}`); profiles.reload()
              })}>Check {profile.name}</button>
              {profile.backend === 'porto' && <button type="button" disabled={busy || profile.revoked} onClick={() => void run(async () => { await apiSend(`/api/environments/${profile.id}/rotation`, 'POST'); refresh() })}>Rotate identity</button>}
              <button type="button" disabled={busy} onClick={() => {
                if (!window.confirm(`Revoke access to ${profile.name} (${profile.id}) and close its active streams? Workload data is retained. Offline remote revocation may need a retry on that host.`)) return
                void run(async () => { try { await apiSend(`/api/environments/${profile.id}/revocation`, 'POST') } finally { refresh() } })
              }}>Revoke {profile.name}</button>
              <button type="button" disabled={busy} onClick={() => {
                const localOnly = profile.backend === 'porto' && !profile.remoteRevoked
                if (!window.confirm(`Remove ${profile.name} (${profile.id}) and its local credential? No workloads or cluster infrastructure are deleted.${localOnly ? ' Remote revocation is unconfirmed: explicitly revoke this identity on its host.' : ''}`)) return
                void run(async () => { await apiSend(`/api/environments/${profile.id}${localOnly ? '?localOnly=true' : ''}`, 'DELETE'); refresh() })
              }}>Remove {profile.name}</button>
            </>}
          </div></td>
        </tr>)}
      </tbody></table></div>
      {metadata && <section className="drawerPanel" aria-label="Environment metadata"><h2>Names and fleet labels</h2><p className="mono">{metadata.id}</p><p>Editing labels never changes the endpoint, identity, capabilities or ownership. Existing fleet previews must be reviewed again.</p><form className="inspectorForm" onSubmit={(event) => {
        event.preventDefault()
        void run(async () => { await apiSend(`/api/environments/${metadata.id}/metadata`, 'PUT', { name: metadata.name, labels: labelsFrom(metadata.labels), revision: metadata.revision }); setMetadata(null); refresh() })
      }}><label>Display name<input required value={metadata.name} onChange={(event) => setMetadata({ ...metadata, name: event.target.value })} /></label><label>Fleet labels and groups (JSON)<textarea value={metadata.labels} onChange={(event) => setMetadata({ ...metadata, labels: event.target.value })} /></label><button type="submit" disabled={busy}>Save environment metadata</button><button type="button" onClick={() => setMetadata(null)}>Cancel metadata edit</button></form></section>}

      <details className="drawerPanel"><summary>Register a remote host, compatible Docker endpoint or external Kubernetes context</summary>
        <p>Verify the server certificate out of band. Secrets are stored only in the OS credential store, with no plaintext fallback. External kubeconfig import never edits global kubeconfigs and rejects executable plugins, file references and insecure TLS.</p>
        <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); add() }}>
          <label>Environment name<input required value={name} onChange={(event) => setName(event.target.value)} /></label>
          <label>Backend<select value={backend} onChange={(event) => { setBackend(event.target.value); setToken(''); setClientKey(''); setKubeconfig('') }}><option value="porto">Authenticated Porto agent</option><option value="docker">Compatible Docker TLS endpoint</option><option value="kubernetes">Explicit external Kubernetes import</option></select></label>
          {backend === 'kubernetes' ? <>
            <label>Imported context name<input required value={context} onChange={(event) => setContext(event.target.value)} /></label>
            <label>Kubeconfig content<textarea required spellCheck={false} value={kubeconfig} onChange={(event) => setKubeconfig(event.target.value)} /></label>
          </> : <>
            <label>HTTPS endpoint<input required type="url" placeholder="https://host.example:37624" value={address} onChange={(event) => setAddress(event.target.value)} /></label>
            <label>Pinned server certificate (PEM)<textarea required spellCheck={false} value={certificate} onChange={(event) => setCertificate(event.target.value)} /></label>
            <label>{backend === 'porto' ? 'Single-use enrollment token' : 'Bearer credential (or use client certificate below)'}<input type="password" autoComplete="off" value={token} onChange={(event) => setToken(event.target.value)} /></label>
            {backend === 'docker' && <><label>Client certificate (PEM)<textarea value={clientCertificate} onChange={(event) => setClientCertificate(event.target.value)} /></label><label>Client private key (PEM)<textarea spellCheck={false} value={clientKey} onChange={(event) => setClientKey(event.target.value)} /></label></>}
          </>}
          {backend !== 'porto' && <label><input type="checkbox" checked={operate} onChange={(event) => setOperate(event.target.checked)} />Allow workload operations, subject to the selected server identity. Infrastructure is not owned.</label>}
          {backend !== 'porto' && <label><input type="checkbox" checked={importSecrets} onChange={(event) => setImportSecrets(event.target.checked)} />Allow sensitive resource inspection and explicit Secret reveals.</label>}
          {backend === 'kubernetes' && <label><input type="checkbox" checked={importTerminal} onChange={(event) => setImportTerminal(event.target.checked)} />Allow explicit pod terminals using this Kubernetes identity.</label>}
          <label>Labels and groups (JSON)<textarea value={labels} onChange={(event) => setLabels(event.target.value)} /></label>
          <button type="submit" disabled={busy}>Register environment</button>
        </form>
      </details>

      <details className="drawerPanel"><summary>Secure listener and outbound Edge enrollment</summary>
        <p>The existing dashboard API remains loopback-only. This separate TLS listener accepts enrolled identities only. Edge agents connect outbound, with heartbeat checks and bounded reconnects. Enrollment expires after five minutes and is single-use.</p>
        {listener.error && <p role="alert">{listener.error}</p>}
        {listener.data ? <><p className="mono">{listener.data.publicAddress}</p><details><summary>Public server certificate to verify and pin</summary><pre className="definitionText">{listener.data.certificate}</pre></details><button type="button" disabled={busy} onClick={() => {
          if (!window.confirm('Stop this secure listener and revoke all enrolled client/Edge identities? Remote workloads remain untouched. Re-enabling requires fresh enrollment and server trust.')) return
          void run(async () => { await apiSend('/api/environments/listener', 'DELETE'); refresh() })
        }}>Stop listener and revoke identities</button></> : <form className="inspectorForm" onSubmit={(event) => {
          event.preventDefault()
          if (!window.confirm(`Enable an authenticated TLS listener at ${listen}, advertised as ${publicAddress}? Only explicitly enrolled identities can use its narrow API.`)) return
          void run(async () => { await apiSend('/api/environments/listener', 'POST', { address: listen, hostname, publicAddress }); refresh() })
        }}><label>Listen address<input required value={listen} onChange={(event) => setListen(event.target.value)} /></label><label>Certificate hostname or IP<input required value={hostname} onChange={(event) => setHostname(event.target.value)} /></label><label>Public HTTPS origin<input required type="url" value={publicAddress} onChange={(event) => setPublicAddress(event.target.value)} /></label><button type="submit" disabled={busy}>Enable secure listener</button></form>}
        <form className="inspectorForm" onSubmit={(event) => {
          event.preventDefault()
          void run(async () => { const result = await apiSend<unknown>('/api/environments/enrollments', 'POST', { name: enrollmentName, kind, capabilities, ttlSeconds: 300 }); setEnrollment(JSON.stringify(result, null, 2)); identities.reload() })
        }}>
          <label>Enrollment name<input required value={enrollmentName} onChange={(event) => setEnrollmentName(event.target.value)} /></label>
          <label>Identity kind<select value={kind} onChange={(event) => setKind(event.target.value)}><option value="client">Inbound authenticated dashboard client</option><option value="edge">Outbound Edge agent</option></select></label>
          <fieldset><legend>Explicit identity capabilities</legend>{CAPABILITIES.map((capability) => <label key={capability}><input type="checkbox" checked={capabilities.includes(capability)} onChange={(event) => setCapabilities((current) => event.target.checked ? [...current, capability] : current.filter((value) => value !== capability))} />{capability}</label>)}</fieldset>
          <button type="submit" disabled={busy || !listener.data}>Create single-use enrollment</button>
        </form>
        {enrollment && <section aria-label="One-time enrollment"><p>Save this privately as an enrollment JSON file on the authorized host. For Edge, run <code>porto agent connect --enrollment /path/to/invite.json</code>. Delete the file after successful enrollment. It is not stored in browser history.</p><pre className="definitionText">{enrollment}</pre><button type="button" onClick={() => setEnrollment('')}>Clear enrollment from view</button></section>}
        {identities.error && <p role="alert">{identities.error}</p>}
        {(identities.data ?? []).map((identity) => <p key={identity.id}>{identity.name} / {identity.kind} / <span className="mono">{identity.id}</span> / {identity.revoked ? 'revoked' : `expires ${new Date(identity.expiresAt).toLocaleString()}`} <button type="button" disabled={identity.revoked || busy} onClick={() => {
          if (!window.confirm(`Revoke identity ${identity.name} (${identity.id}) and terminate its active access?`)) return
          void run(async () => { await apiSend(`/api/environments/identities/${identity.id}`, 'DELETE'); refresh() })
        }}>Revoke identity</button></p>)}
      </details>

      <section className="drawerPanel" aria-label="Fleet operations"><h2>Explicit multi-environment container actions</h2><p>Select exact environments and a container name or ID on each. Preview resolves immutable container IDs. Concurrent actions have per-target results and cancellation; offline targets are never retried automatically.</p>
        <form className="inspectorForm" onChange={() => { setPreview(null); setConfirmation('') }} onSubmit={(event) => {
          event.preventDefault()
          void run(async () => { const value = await apiSend<FleetPreview>('/api/environments/fleet/previews', 'POST', { action, targets: Object.entries(targets).filter(([, resourceId]) => resourceId.trim()).map(([environmentId, resourceId]) => ({ environmentId, resourceId })) }); setPreview(value); setConfirmation('') })
        }}>
          {(profiles.data ?? []).filter((profile) => !profile.revoked && profile.backend !== 'kubernetes').map((profile) => <label key={profile.id}>Container on {profile.name} ({profile.id})<input value={targets[profile.id] ?? ''} placeholder="Leave empty to exclude this target" onChange={(event) => setTargets((current) => ({ ...current, [profile.id]: event.target.value }))} /></label>)}
          <label>Fleet action<select value={action} onChange={(event) => setAction(event.target.value)}>{['start', 'stop', 'restart', 'remove'].map((value) => <option key={value}>{value}</option>)}</select></label><button type="submit" disabled={busy}>Preview fleet targets</button>
        </form>
        {preview && <section aria-label="Fleet target preview"><pre className="definitionText">{JSON.stringify(preview.targets, null, 2)}</pre><p>Expires {new Date(preview.expiresAt).toLocaleTimeString()}. Removing a container does not delete its volumes.</p><label>Type confirmation: {preview.confirmation}<input value={confirmation} onChange={(event) => setConfirmation(event.target.value)} /></label><button type="button" disabled={busy || confirmation !== preview.confirmation} onClick={() => void run(async () => { await apiSend('/api/environments/fleet/runs', 'POST', { preview: preview.id, confirmation }); setPreview(null); runs.reload() })}>Execute reviewed fleet action</button></section>}
        {runs.error && <p role="alert">{runs.error}</p>}
        {(runs.data ?? []).map((value) => <details key={value.id}><summary>{value.action} / {value.id}</summary>{value.results.map((result) => <p key={`${result.target.environmentId}:${result.target.resourceId}`}>{result.target.environmentName} / {result.target.resourceId} / <strong>{result.status}</strong> {result.error}</p>)}{value.results.some((result) => result.status === 'pending') && <button type="button" onClick={() => void run(async () => { await apiSend(`/api/environments/fleet/runs/${value.id}/cancel`, 'POST'); runs.reload() })}>Cancel fleet run</button>}</details>)}
      </section>
    </div>
  </>
}
