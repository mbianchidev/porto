import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import type { WSLStatus } from '../platformTypes'
import { useMessages } from '../useMessages'

export function WSLSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const state = usePolledResource(async (signal) => {
    const status = await apiGet<WSLStatus>('/api/wsl', signal)
    if (!status || Array.isArray(status) || !Array.isArray(status.distributions)) {
      throw new Error('WSL integration state is unavailable. Restart the matching Porto daemon, then refresh.')
    }
    return status
  }, 15000, [], 'platform:wsl')
  const [roots, setRoots] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const pending = useRef(false)
  const status = state.data

  async function perform(path: string, body: unknown, message: string) {
    if (pending.current) return
    pending.current = true
    setBusy(true)
    setFailure('')
    try {
      await apiSend(path, 'POST', body)
      state.reload()
      notifyNotice('wsl', message)
    } catch (err) {
      const message = errorMessage(err, 'Selected WSL integration failed. Check distribution ownership and retry.')
      setFailure(message)
      notifyError('wsl', message)
    } finally {
      pending.current = false
      setBusy(false)
    }
  }

  function enable(name: string) {
    if (!window.confirm(`Enable or repair Porto tooling only in "${name}"? Its user home and Windows user home become selected shared roots. Only owned tools and shell hooks are installed. Existing Docker configuration, other distributions, and the Linux engine are preserved.`)) return
    void perform(`/api/wsl/distributions/${encodeURIComponent(name)}/enable`,
      { mountRoots: roots.split('\n').map((root) => root.trim()).filter(Boolean) },
      `Porto tooling enabled for ${name}. Open a new shell; apply engine mounts explicitly when ready.`)
  }

  return (
    <section className="integration platformSettings" aria-labelledby="wsl-settings-title">
      <div className="hygieneIntro">
        <h2 id="wsl-settings-title">Selected WSL distributions</h2>
        <p>Native Linux Docker, Compose, and Buildx connect through a private distribution socket to the Windows Docker pipe. No TCP control API is opened.</p>
        <p className="hintLine">This is development tooling for the existing Linux engine, not a Windows-container runtime or a switch to Lima's WSL driver.</p>
      </div>
      <div className="hygieneControls">
        {(failure || state.error) && <p className="errorLine" role="alert">{failure || state.error}</p>}
        {state.loading && !status && <p role="status">Reading selected WSL distributions...</p>}
        {status && !status.supported && <p>{status.message || 'WSL integration requires Windows.'}</p>}
        {status?.supported && (
          <>
            {status.distributions.length === 0 && <p>No WSL distributions are registered. Install a WSL 2 distribution explicitly, then refresh.</p>}
            <div className="platformDistributionList">
              {status.distributions.map((distribution) => (
                <article className="platformDistribution" key={distribution.id}>
                  <div><strong>{distribution.name}</strong><p className="hintLine">
                    WSL {distribution.version}; {distribution.running ? 'running' : 'stopped'}; {distribution.enabled ? 'opted in' : 'not opted in'}
                  </p>{distribution.message && <p className="hintLine">{distribution.message}</p>}</div>
                  <div className="platformActions">
                    <button type="button" disabled={busy || distribution.version !== 2} onClick={() => enable(distribution.name)}>
                      {distribution.enabled ? 'Repair' : 'Enable'} {distribution.name}
                    </button>
                    {distribution.enabled && <button type="button" className="destructiveAction" disabled={busy} onClick={() => {
                      if (!window.confirm(`Remove only Porto tooling from "${distribution.name}"? Its files, engine data, other distributions, and Windows vault credentials are preserved. Apply engine mounts separately to release old shares.`)) return
                      void perform(`/api/wsl/distributions/${encodeURIComponent(distribution.name)}/disable`, undefined, `Porto tooling removed only from ${distribution.name}.`)
                    }}>Remove {distribution.name} tooling</button>}
                  </div>
                </article>
              ))}
            </div>
            <label className="settingsField">
              <span>Extra shared Linux roots</span>
              <textarea rows={2} value={roots} disabled={busy} onChange={(event) => setRoots(event.target.value)} placeholder="/workspace (one absolute path per line)" />
              <small>Applied only to the distribution you enable or repair. Shared bind watches use polling; explicit application watch settings are preserved.</small>
            </label>
            <button type="button" disabled={busy} onClick={() => {
              if (!window.confirm('Apply only selected WSL mounts to the existing Porto Linux engine? This restarts that owned engine and interrupts its running workloads. Save work first. No engine, image, container, or volume data is replaced.')) return
              void perform('/api/wsl/mounts', { restartEngine: true }, 'Selected WSL mounts applied to the owned Linux engine.')
            }}>Apply WSL mounts</button>
            <p className="hintLine">Linux file paths are mapped only within selected roots. Docker credentials stay in the Windows native vault. Non-opted-in distributions and global WSL settings are not changed.</p>
          </>
        )}
        <button type="button" disabled={busy} onClick={state.reload}>Refresh WSL distributions</button>
      </div>
    </section>
  )
}
