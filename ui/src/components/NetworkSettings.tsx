import { useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import type { ConnectivityReport, NetworkStatus, ProxyConfig, ProxyStatus } from '../platformTypes'
import { useMessages } from '../useMessages'

function editableProxy(proxy: ProxyStatus): ProxyConfig {
  return {
    mode: proxy.mode, httpProxy: proxy.httpProxy, httpsProxy: proxy.httpsProxy,
    socksProxy: proxy.socksProxy, noProxy: proxy.noProxy, registryCAs: [...proxy.registryCAs],
  }
}

export function NetworkSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const network = usePolledResource(async (signal) => {
    const status = await apiGet<NetworkStatus>('/api/networking', signal)
    if (!status || Array.isArray(status) || !status.https || !status.proxy) {
      throw new Error('Networking state is unavailable. Restart the matching Porto daemon, then refresh.')
    }
    return status
  }, 15000, [], 'platform:networking')
  const [prior, setPrior] = useState<NetworkStatus | null>(null)
  const [draft, setDraft] = useState<ProxyConfig | null>(null)
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const pending = useRef(false)
  const [failure, setFailure] = useState('')
  const [connectivity, setConnectivity] = useState<ConnectivityReport | null>(null)
  const status = network.data

  if (status && status !== prior) {
    setPrior(status)
    if (!dirty) setDraft(editableProxy(status.proxy))
  }

  function edit<Key extends keyof ProxyConfig>(key: Key, value: ProxyConfig[Key]) {
    setDraft((current) => current ? { ...current, [key]: value } : current)
    setDirty(true)
  }

  async function perform(action: () => Promise<void>) {
    if (pending.current) return
    pending.current = true
    setBusy(true)
    setFailure('')
    try {
      await action()
    } catch (err) {
      const message = errorMessage(err, 'Networking action failed. Review the platform status and retry.')
      setFailure(message)
      notifyError('networking', message)
    } finally {
      pending.current = false
      setBusy(false)
    }
  }

  function httpsAction(action: 'install' | 'uninstall' | 'trust' | 'untrust') {
    const description = action === 'install'
      ? 'Install a loopback-only HTTPS forwarder and trust the Porto CA? Only platform setup may request administrator authorization. The daemon and workloads stay unprivileged.'
      : action === 'uninstall'
        ? 'Remove only the Porto HTTPS forwarder? Certificate trust remains until you remove trust separately.'
        : action === 'untrust'
          ? 'Remove only this Porto CA from the native trust store? Local HTTPS will show certificate warnings afterward.'
          : 'Trust this Porto certificate authority in the native trust store? Other certificates are not changed.'
    if (!window.confirm(description)) return
    void perform(async () => {
      const next = await apiSend<NetworkStatus>(`/api/tls/https/${action}`, 'POST')
      network.update(() => next)
      network.reload()
      notifyNotice('networking', `HTTPS ${action} completed.`)
    })
  }

  function save(event: FormEvent) {
    event.preventDefault()
    if (!draft) return
    void perform(async () => {
      const saved = await apiSend<ProxyStatus>('/api/networking/proxy', 'PUT', draft)
      setDraft(editableProxy(saved))
      setDirty(false)
      network.update((current) => current ? { ...current, proxy: saved } : current)
      network.reload()
      notifyNotice('networking', 'Network settings saved. Apply them to the owned engine after finishing active builds.')
    })
  }

  function apply() {
    if (!window.confirm('Apply proxy and registry CA settings to Porto\'s owned engine? This restarts only its idle BuildKit service. Active builds block the change. Workload trust stores are not modified.')) return
    void perform(async () => {
      await apiSend('/api/networking/apply', 'POST')
      network.reload()
      notifyNotice('networking', 'Owned engine network settings applied.')
    })
  }

  return (
    <section className="integration platformSettings" aria-labelledby="network-settings-title">
      <div className="hygieneIntro">
        <h2 id="network-settings-title">Trusted local HTTPS</h2>
        <p>Browser to loopback port 443 to the unprivileged Porto router. Never expose the control API to the network.</p>
        <p className="platformPath"><code>127.0.0.1 / ::1 :443</code><span>to</span><code>127.0.0.1 :37681</code></p>
        <p className="hintLine">Chrome and Edge use native trust on Windows and macOS. Linux setup includes Chromium's user NSS store; Firefox, Snap, and Flatpak profiles may need a separate import. No TLS validation is disabled.</p>
      </div>
      <div className="hygieneControls">
        {(failure || network.error) && <p className="errorLine" role="alert">{failure || network.error}</p>}
        {network.loading && !status && <p role="status">Reading network integration...</p>}
        {status && (
          <>
            <p role="status">
              {status.https.installed && status.https.listening && status.https.trusted
                ? 'Installed, listening, and trusted.'
                : `Forwarder ${status.https.installed ? 'installed' : 'not installed'}; ${status.https.listening ? 'listening' : 'not listening'}; certificate ${status.https.trusted ? 'trusted' : 'not trusted'}.`}
            </p>
            {status.https.message && <p className="hintLine">{status.https.message}</p>}
            <div className="platformActions">
              <button type="button" disabled={busy || !status.https.supported} onClick={() => httpsAction('install')}>
                {status.https.installed ? 'Repair trusted HTTPS' : 'Install trusted HTTPS'}
              </button>
              <button type="button" disabled={busy || !status.https.installed} onClick={() => httpsAction('uninstall')}>Uninstall HTTPS helper</button>
              <button type="button" disabled={busy || !status.https.supported} onClick={() => httpsAction(status.https.trusted ? 'untrust' : 'trust')}>
                {status.https.trusted ? 'Remove certificate trust' : 'Trust certificate authority'}
              </button>
              <button type="button" disabled={busy} onClick={() => void perform(async () => {
                await apiSend('/api/tls/renew', 'POST')
                network.reload()
                notifyNotice('networking', 'HTTPS server certificate renewed without replacing the CA.')
              })}>Renew HTTPS certificate</button>
            </div>
            <h3>Proxy and registry trust</h3>
            {!status.proxyAvailable && <p className="errorLine" role="alert">{status.proxy.message || 'Proxy settings are unavailable. Configure an explicit proxy or choose off.'}</p>}
            <p className="hintLine">Source: {status.proxy.source || 'unavailable'}. Local Porto routes always bypass proxies. PAC/WPAD requires an external resolver or explicit proxy URLs.</p>
            {draft && (
              <form className="platformForm" onSubmit={save}>
                <label className="settingsField">
                  <span>Proxy mode</span>
                  <select value={draft.mode} disabled={busy} onChange={(event) => {
                    const mode = event.target.value
                    if (mode === 'system' || mode === 'manual' || mode === 'off') edit('mode', mode)
                  }}>
                    <option value="system">System and environment</option>
                    <option value="manual">Explicit proxy URLs</option>
                    <option value="off">Proxy off</option>
                  </select>
                </label>
                {([
                  ['httpProxy', 'HTTP proxy URL', 'http'],
                  ['httpsProxy', 'HTTPS proxy URL', 'https'],
                  ['socksProxy', 'SOCKS proxy URL', 'socks'],
                ] as const).map(([key, label, credential]) => (
                  <label className="settingsField" key={key}>
                    <span>{label}</span>
                    <input type="password" autoComplete="off" spellCheck={false} value={draft[key]} disabled={busy || draft.mode !== 'manual'} onChange={(event) => edit(key, event.target.value)} />
                    <small>{status.proxy.credentialsStored[credential]
                      ? 'Credential saved in the native vault. Leave the URL unchanged to keep it.'
                      : 'Optional URL credentials are stored in the native vault, not in configuration or diagnostics.'}</small>
                  </label>
                ))}
                <label className="settingsField">
                  <span>Proxy bypass rules</span>
                  <input type="text" value={draft.noProxy} disabled={busy || draft.mode !== 'manual'} onChange={(event) => edit('noProxy', event.target.value)} placeholder=".internal.example,10.0.0.0/8" />
                </label>
                <label className="settingsField">
                  <span>Trusted registry CA files</span>
                  <textarea rows={3} value={draft.registryCAs.join('\n')} disabled={busy} onChange={(event) => edit('registryCAs', event.target.value.split('\n').filter((path) => path.trim() !== ''))} placeholder="One public PEM CA file path per line" />
                  <small>Copied into owned registry trust. Remove paths and apply again to reverse. The Porto development CA is never added to workloads automatically.</small>
                </label>
                <label className="toggleRow">
                  <span>Clear saved proxy credentials on save</span>
                  <input type="checkbox" checked={draft.clearCredentials ?? false} disabled={busy} onChange={(event) => edit('clearCredentials', event.target.checked)} />
                </label>
                <div className="platformActions">
                  <button type="submit" disabled={busy}>Save network settings</button>
                  <button type="button" disabled={busy || dirty || !status.engine?.configured} onClick={apply}>Apply to owned engine</button>
                </div>
              </form>
            )}
            {status.engine?.needsApply && <p className="hintLine">Owned engine settings need applying. No running builder was restarted automatically.</p>}
            {status.engineError && <p className="errorLine" role="alert">{status.engineError}</p>}
            {status.warnings.map((warning) => <p className="hintLine" key={warning}>{warning}</p>)}
            {status.routes.length > 0 && (
              <ul className="platformRoutes">
                {status.routes.map((route) => <li key={route.containerId}><a href={route.httpsURL} target="_blank" rel="noreferrer">{route.hostname}.porto.localhost</a></li>)}
              </ul>
            )}
          </>
        )}
        <div className="platformActions">
          <button type="button" disabled={busy} onClick={network.reload}>Refresh networking</button>
          <button type="button" disabled={busy} onClick={() => void perform(async () => {
            setConnectivity(await apiGet<ConnectivityReport>('/api/networking/connectivity'))
          })}>Inspect connectivity</button>
        </div>
        {connectivity && (
          <div className="platformConnectivity">
            <p>{connectivity.containerDirectIP.message}</p>
            <p>{connectivity.vmDirectIP.message}</p>
            {connectivity.host.warnings.map((warning) => <p className="hintLine" key={warning}>{warning}</p>)}
            {connectivity.networks.map((network) => <div key={network.name}>
              <strong>{network.name}</strong><span> {network.subnets.join(', ')}</span>
              {network.overlaps.map((overlap) => <p className="hintLine" key={`${overlap.subnet}:${overlap.interface}:${overlap.hostCIDR}`}>
                Overlap candidate: {overlap.subnet} and {overlap.hostCIDR} on {overlap.interface}. An owned bridge overlap may be intentional. No routes were changed.
              </p>)}
            </div>)}
            {(connectivity.virtualMachines ?? []).map((machine) => <p key={machine.name}>
              <strong>{machine.name}</strong>: {machine.addresses?.join(', ') || 'no direct address reported'}
              {machine.sshLocalPort > 0 && <>; SSH through 127.0.0.1:{machine.sshLocalPort}</>}
            </p>)}
          </div>
        )}
      </div>
    </section>
  )
}
