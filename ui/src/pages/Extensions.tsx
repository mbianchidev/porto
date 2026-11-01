import { useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ExtensionTools } from '../components/ExtensionContributions'
import type { EnvironmentProfile, ExtensionCatalogEntry, ExtensionGrants, InstalledExtension } from '../managementTypes'

const EMPTY_GRANTS: ExtensionGrants = { permissions: [], environments: [], files: [], commands: [], secrets: [], destinations: [] }
function lines(text: string) { return text.split('\n').map((line) => line.trim()).filter(Boolean) }
function commands(text: string): ExtensionGrants['commands'] {
  const value: unknown = JSON.parse(text)
  if (!Array.isArray(value)) throw new Error('Commands must be a JSON array of executable/args objects.')
  return value.map((entry: unknown) => {
    if (!entry || typeof entry !== 'object' || !('executable' in entry) || typeof entry.executable !== 'string' || !('args' in entry) || !Array.isArray(entry.args) || entry.args.some((arg: unknown) => typeof arg !== 'string')) throw new Error('Each command needs an absolute executable and an exact string argument array.')
    return { executable: entry.executable, args: entry.args as string[] }
  })
}

export function Extensions() {
  const { notifyError, notifyNotice } = useMessages()
  const installed = usePolledResource<InstalledExtension[]>((signal) => apiGet('/api/extensions', signal), 3000, [])
  const catalog = usePolledResource<ExtensionCatalogEntry[]>((signal) => apiGet('/api/extensions/catalog', signal), 0, [])
  const publishers = usePolledResource<Record<string, string>>((signal) => apiGet('/api/extensions/publishers', signal), 0, [])
  const environments = usePolledResource<EnvironmentProfile[]>((signal) => apiGet('/api/environments', signal), 10000, [])
  const [packageText, setPackageText] = useState('')
  const [expectedSHA256, setExpectedSHA256] = useState('')
  const [developer, setDeveloper] = useState(false)
  const [selectedID, setSelectedID] = useState('')
  const [permissions, setPermissions] = useState<string[]>([])
  const [targets, setTargets] = useState<string[]>([])
  const [files, setFiles] = useState('')
  const [commandText, setCommandText] = useState('[]')
  const [secretNames, setSecretNames] = useState('')
  const [destinations, setDestinations] = useState('')
  const [approve, setApprove] = useState(false)
  const [busy, setBusy] = useState(false)
  const [publisherKey, setPublisherKey] = useState('')
  const [publisherLabel, setPublisherLabel] = useState('')
  const [catalogText, setCatalogText] = useState('')
  const [secretName, setSecretName] = useState('')
  const [secret, setSecret] = useState('')
  const selected = (installed.data ?? []).find((item) => item.id === selectedID)

  async function run(work: () => Promise<void>) {
    if (busy) return
    setBusy(true)
    try { await work() } catch (error) { notifyError('extensions', errorMessage(error, 'Extension management failed')) } finally { setBusy(false) }
  }
  function select(value: InstalledExtension) {
    setSelectedID(value.id)
    const grants = value.grants ?? EMPTY_GRANTS
    setPermissions(grants.permissions ?? []); setTargets(grants.environments ?? []); setFiles((grants.files ?? []).join('\n'))
    setCommandText(JSON.stringify(grants.commands ?? [], null, 2)); setSecretNames((grants.secrets ?? []).join('\n')); setDestinations((grants.destinations ?? []).join('\n')); setApprove(false); setSecret('')
  }
  function refresh() { installed.reload(); catalog.reload(); publishers.reload() }
  async function readFile(file: File | undefined, setter: (value: string) => void) {
    if (!file) return
    try {
      if (file.size > 16 * 1024 * 1024) throw new Error('Extension files are limited to 16 MiB.')
      setter(await file.text())
    } catch (error) { notifyError('extensions', errorMessage(error, 'Unable to read selected extension file')) }
  }

  return <>
    <section className="fleetRail" aria-label="Extension host status"><span className="fleetRailTitle">Permission-scoped extensions</span><span className="fleetDatum">{installed.data?.length ?? 0} installed</span><span className="fleetMessage">Verified artifacts. Explicit grants. Static sandboxed views and bounded WebAssembly actions.</span></section>
    <div className="controlBar"><button type="button" onClick={refresh}>Refresh extensions</button><a href="https://github.com/mbianchidev/porto/blob/main/docs/extensions.md" target="_blank" rel="noreferrer">Authoring and API compatibility</a></div>
    <div className="workArea managementArea">
      {installed.error && <p role="alert">{installed.error}</p>}
      <div className="dataTableScroll"><table className="dataTable"><caption>Installed tooling</caption><thead><tr><th>Identity and provenance</th><th>Status and permissions</th><th>Controls</th></tr></thead><tbody>
        {(installed.data ?? []).map((value) => <tr key={value.id}><td><strong>{value.name}</strong><p>{value.id} / {value.version}</p><p>{value.source} / {value.publisher ? publishers.data?.[value.publisher] || 'Unknown publisher' : 'Unsigned developer package'}</p><p className="mono">{value.sha256}</p></td>
          <td><strong>{value.state}</strong><p>{value.compatible ? 'API compatible' : 'Incompatible API'}</p><p>Requested: {value.manifest.permissions.join(', ') || 'No host capabilities'}</p><p>Granted: {value.grants.permissions?.join(', ') || 'None'}</p>{value.lastError && <p role="alert">{value.lastError}</p>}</td>
          <td><div className="actions"><button type="button" onClick={() => select(value)}>Review {value.name}</button><button type="button" disabled={busy || !value.enabled} onClick={() => void run(async () => { await apiSend(`/api/extensions/${value.id}/configuration`, 'PUT', { enabled: false, approve: false, grants: EMPTY_GRANTS }); installed.reload() })}>Disable {value.name}</button><button type="button" disabled={busy} onClick={() => {
            if (!window.confirm(`Remove ${value.name}, cancel its running actions and delete its owned credentials/packages? Workloads and explicitly accessed host files are retained.`)) return
            void run(async () => { await apiSend(`/api/extensions/${value.id}`, 'DELETE'); if (selectedID === value.id) setSelectedID(''); refresh() })
          }}>Remove {value.name}</button></div></td>
        </tr>)}
      </tbody></table></div>
      {!(installed.data ?? []).length && <p>No extensions installed. Build the separately packaged resource-summary reference integration from the authoring guide, or import a trusted publisher's signed catalog. Nothing is executed by discovering files in a repository.</p>}

      <details className="drawerPanel"><summary>Install or update a verified package</summary><p>Packages remain disabled until you review grants. Updates must retain publisher provenance and increase the version; all grants are reset. Compare SHA-256 with a trusted source, not only the value inside the package.</p>
        <form className="inspectorForm" onSubmit={(event) => {
          event.preventDefault()
          void run(async () => { const pkg: unknown = JSON.parse(packageText); const value = await apiSend<InstalledExtension>('/api/extensions', 'POST', { package: pkg, expectedSHA256, developer }); setPackageText(''); setExpectedSHA256(''); setDeveloper(false); installed.reload(); select(value); notifyNotice('extensions', 'Verified package installed, disabled and awaiting explicit grants.') })
        }}><label>Extension package file<input type="file" accept=".porto-extension,.json" onChange={(event) => void readFile(event.target.files?.[0], setPackageText)} /></label><label>Independently verified SHA-256<input required pattern="[a-f0-9]{64}" value={expectedSHA256} onChange={(event) => setExpectedSHA256(event.target.value)} /></label><label><input type="checkbox" checked={developer} onChange={(event) => setDeveloper(event.target.checked)} />I explicitly accept an unsigned local/developer package. This is not a trusted catalog install.</label><button type="submit" disabled={busy || !packageText}>Install or update package</button></form>
      </details>

      {selected && <section className="drawerPanel" aria-label="Extension grants"><h2>{selected.name}: explicit grants</h2><p>Capabilities apply to this installation only. Changing grants, disabling or updating closes active module/broker handles. Host-file and process access refers to this dashboard's host, not a selected remote environment.</p>
        <form className="inspectorForm" onChange={() => setApprove(false)} onSubmit={(event) => {
          event.preventDefault()
          void run(async () => {
            const grants: ExtensionGrants = { permissions, environments: targets, files: lines(files), commands: commands(commandText), secrets: lines(secretNames), destinations: lines(destinations) }
            await apiSend(`/api/extensions/${selected.id}/configuration`, 'PUT', { enabled: true, approve, grants })
            setApprove(false); installed.reload(); notifyNotice('extensions', 'Extension enabled with the reviewed capabilities and exact scopes.')
          })
        }}>
          <fieldset><legend>Requested capabilities to approve</legend>{selected.manifest.permissions.map((permission) => <label key={permission}><input type="checkbox" checked={permissions.includes(permission)} onChange={(event) => setPermissions((current) => event.target.checked ? [...current, permission] : current.filter((value) => value !== permission))} />{permission}</label>)}</fieldset>
          <fieldset><legend>Authorized runtime environments</legend>{(environments.data ?? []).filter((value) => !value.revoked).map((value) => <label key={value.id}><input type="checkbox" checked={targets.includes(value.id)} onChange={(event) => setTargets((current) => event.target.checked ? [...current, value.id] : current.filter((id) => id !== value.id))} />{value.name} / {value.id}</label>)}</fieldset>
          {(permissions.includes('host.files.read') || permissions.includes('host.files.write')) && <label>Exact existing host files (absolute paths, one per line)<textarea value={files} onChange={(event) => setFiles(event.target.value)} /></label>}
          {permissions.includes('process.exec') && <label>Exact native executable/argument grants (JSON)<textarea value={commandText} onChange={(event) => setCommandText(event.target.value)} /><span>Native programs have the host user's privileges. Grant only exact reviewed commands; they are not WebAssembly-sandboxed. Time, output and process-group cleanup are bounded.</span></label>}
          {permissions.includes('secrets.read') && <label>Extension-owned secret names (one per line)<textarea value={secretNames} onChange={(event) => setSecretNames(event.target.value)} /></label>}
          {permissions.includes('network.fetch') && <label>Exact HTTPS origins (one per line)<textarea value={destinations} onChange={(event) => setDestinations(event.target.value)} /><span>No redirects, private addresses, ambient credentials or arbitrary sockets.</span></label>}
          <button type="submit" disabled={busy || !approve}>Approve grants and enable</button>
        </form>
        <label><input type="checkbox" checked={approve} onChange={(event) => setApprove(event.target.checked)} />I reviewed these capabilities and their exact environment/file/process/secret/network scopes.</label>
        <details><summary>Store a named extension-owned secret</summary><form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void run(async () => { await apiSend(`/api/extensions/${selected.id}/secrets`, 'POST', { name: secretName, value: secret }); setSecret(''); installed.reload() }) }}><label>Secret name<input required value={secretName} onChange={(event) => setSecretName(event.target.value)} /></label><label>Secret value<input type="password" autoComplete="off" required value={secret} onChange={(event) => setSecret(event.target.value)} /></label><button type="submit" disabled={busy}>Store in OS credential store</button></form></details>
        <ExtensionTools key={`${selected.id}:${selected.generation}`} installation={selected} />
      </section>}

      <details className="drawerPanel"><summary>Trusted publishers and signed catalog discovery</summary><p>Trust is an explicit local decision, not a claim that code is safe. Verify publisher keys out of band. Importing an index never installs, enables or executes its entries.</p>
        {publishers.error && <p role="alert">{publishers.error}</p>}
        <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); if (!window.confirm(`Trust the independently verified Ed25519 key for ${publisherLabel}? Packages still require digest verification and explicit grants.`)) return; void run(async () => { await apiSend('/api/extensions/publishers', 'POST', { publicKey: publisherKey, label: publisherLabel }); publishers.reload() }) }}><label>Publisher label<input required value={publisherLabel} onChange={(event) => setPublisherLabel(event.target.value)} /></label><label>Verified Ed25519 public key (base64)<input required value={publisherKey} onChange={(event) => setPublisherKey(event.target.value)} /></label><button type="submit" disabled={busy}>Trust verified publisher</button></form>
        {Object.entries(publishers.data ?? {}).map(([key, label]) => <p key={key}>{label} <code>{key}</code> <button type="button" disabled={busy} onClick={() => { if (!window.confirm(`Revoke trust for ${label} and disable all its extensions?`)) return; void run(async () => { await apiSend('/api/extensions/publishers', 'POST', { publicKey: key, revoke: true }); refresh() }) }}>Revoke publisher</button></p>)}
        <form className="inspectorForm" onSubmit={(event) => { event.preventDefault(); void run(async () => { const value: unknown = JSON.parse(catalogText); await apiSend('/api/extensions/catalog', 'POST', value); setCatalogText(''); catalog.reload() }) }}><label>Signed catalog index file<input type="file" accept=".json" onChange={(event) => void readFile(event.target.files?.[0], setCatalogText)} /></label><button type="submit" disabled={busy || !catalogText}>Import signed catalog</button></form>
      </details>
      <section className="drawerPanel"><h2>Discover approved tooling packages</h2>{catalog.error && <p role="alert">{catalog.error}</p>}{!(catalog.data ?? []).length && <p>No signed catalog imported. Porto does not contact a marketplace or require an account.</p>}{(catalog.data ?? []).map((entry) => <section key={`${entry.publisher}:${entry.id}:${entry.sha256}`}><h3>{entry.name} / {entry.version}</h3><p>{entry.description}</p><p>Publisher: {publishers.data?.[entry.publisher] || entry.publisher}</p><p className="mono">{entry.id} / {entry.sha256}</p><button type="button" disabled={busy} onClick={() => {
        if (!window.confirm(`Download and verify ${entry.id} ${entry.version} from ${entry.url}, pinned to ${entry.sha256}? It remains disabled until grant approval.`)) return
        void run(async () => { await apiSend('/api/extensions/catalog/install', 'POST', { id: entry.id, sha256: entry.sha256, publisher: entry.publisher }); installed.reload() })
      }}>Install catalog package</button></section>)}</section>
    </div>
  </>
}
