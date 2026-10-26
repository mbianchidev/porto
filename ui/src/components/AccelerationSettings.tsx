import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { ARCHITECTURE_POLICIES, isArchitecturePolicy } from '../acceleration'
import type { AccelerationSettingsValue, AccelerationStatus } from '../acceleration'

export function AccelerationSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const status = usePolledResource(async (signal) => {
    const result = await apiGet<AccelerationStatus>('/api/runtime/acceleration', signal)
    if (!result?.settings || !result.capabilities) throw new Error('Architecture capabilities are unavailable. Restart the matching Porto daemon.')
    return result
  }, 30000, [], 'runtime:acceleration')
  const [prior, setPrior] = useState<AccelerationStatus | null>(null)
  const [draft, setDraft] = useState<AccelerationSettingsValue | null>(null)
  const [dirty, setDirty] = useState(false)
  const [busy, setBusy] = useState(false)
  const pending = useRef(false)
  if (status.data && status.data !== prior) {
    setPrior(status.data)
    if (!dirty) setDraft(status.data.settings)
  }
  const capabilities = status.data?.capabilities

  async function perform(action: () => Promise<void>) {
    if (pending.current) return
    pending.current = true
    setBusy(true)
    try {
      await action()
    } catch (error) {
      notifyError('acceleration', errorMessage(error, 'Architecture configuration failed'))
    } finally {
      pending.current = false
      setBusy(false)
    }
  }

  return (
    <section className="integration platformSettings" aria-labelledby="acceleration-title">
      <div className="hygieneIntro">
        <h2 id="acceleration-title">Architecture and acceleration</h2>
        <p>Native workloads stay native. Choose how foreign Linux userspace runs inside Porto's engine.</p>
        <p className="hintLine">Rosetta is opt-in and requires an arm64 VZ guest. It cannot accelerate an amd64 kernel. Full amd64 VMs on Apple Silicon use QEMU software emulation.</p>
        <p className="hintLine">Host tools and translation handlers outside Porto's owned guest are never installed automatically.</p>
      </div>
      <div className="hygieneControls">
        {status.error && <p className="errorLine" role="alert">{status.error}</p>}
        {capabilities && (
          <>
            <dl className="runtimeGrid">
              <div><dt>Host</dt><dd>{capabilities.hostOS}/{capabilities.hostArchitecture}</dd></div>
              <div><dt>Engine</dt><dd>{capabilities.guestArchitecture || 'unavailable'} / {capabilities.vmType || 'external'}</dd></div>
              <div><dt>Lima</dt><dd>{capabilities.limaVersion || 'not installed'}</dd></div>
              <div><dt>amd64 handler</dt><dd>{capabilities.amd64Handler || 'not registered'}</dd></div>
              <div><dt>Virtualization.framework</dt><dd>{capabilities.virtualization.supported ? 'available' : capabilities.virtualization.reason}</dd></div>
              <div><dt>Rosetta on host</dt><dd>{capabilities.rosettaHost.supported ? 'installed' : capabilities.rosettaHost.reason}</dd></div>
            </dl>
            {status.data?.message && <p className="hintLine">{status.data.message}</p>}
          </>
        )}
        {draft && (
          <form className="platformForm" onSubmit={(event) => {
            event.preventDefault()
            void perform(async () => {
              const result = await apiSend<AccelerationStatus>('/api/runtime/acceleration', 'PUT', draft)
              setDirty(false)
              status.update(() => result)
              notifyNotice('acceleration', 'Architecture policy saved. Translator changes require applying it to the idle engine.')
            })
          }}>
            <label className="settingsField">
              <span>Architecture policy</span>
              <select value={draft.policy} disabled={busy} onChange={(event) => {
                const value = event.target.value
                if (isArchitecturePolicy(value)) {
                  setDraft({ ...draft, policy: value, rosettaEnabled: value === 'native-only' || value === 'qemu-compatible' ? false : draft.rosettaEnabled })
                  setDirty(true)
                }
              }}>
                {ARCHITECTURE_POLICIES.map((policy) => <option key={policy} value={policy}>{policy}</option>)}
              </select>
            </label>
            <label className="toggleRow">
              <span><strong>Use Rosetta in the owned engine</strong><small>Already-installed Apple software only. A driver change or disk replacement is never attempted.</small></span>
              <input type="checkbox" checked={draft.rosettaEnabled} disabled={busy || !capabilities?.rosettaHost.supported || draft.policy === 'native-only' || draft.policy === 'qemu-compatible'} onChange={(event) => {
                setDraft({ ...draft, rosettaEnabled: event.target.checked })
                setDirty(true)
              }} />
            </label>
            <div className="platformActions">
              <button type="submit" disabled={busy || !dirty}>Save architecture policy</button>
              <button type="button" disabled={busy || dirty || !capabilities?.guestArchitecture} onClick={() => {
                if (!window.confirm('Restart only the idle Porto-owned engine to apply its translation policy? Active builds, running or paused tasks, and incomplete inventories block the change. Its existing disk is preserved.')) return
                void perform(async () => {
                  await apiSend('/api/runtime/acceleration/apply', 'POST', { restartEngine: true })
                  status.reload()
                  notifyNotice('acceleration', 'Engine architecture policy applied.')
                })
              }}>Apply to idle engine</button>
              <button type="button" disabled={busy} onClick={status.reload}>Refresh capabilities</button>
            </div>
          </form>
        )}
      </div>
    </section>
  )
}
