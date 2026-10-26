import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import { isSaverMode, parseSaverOverrides, SAVER_MODES } from '../resourceManagement'
import type { EngineLimits, EngineResourceStatus, SaverConfig, SaverStatus } from '../resourceManagement'
import type { DockerContainer } from '../types'

function useResourceAction() {
  const { notifyError, notifyNotice } = useMessages()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const pending = useRef(false)
  async function run(action: () => Promise<void>) {
    if (pending.current) return
    pending.current = true
    setBusy(true)
    setError('')
    try {
      await action()
    } catch (error) {
      const message = errorMessage(error, 'Resource management action failed')
      setError(message)
      notifyError('resources', message)
    } finally {
      setBusy(false)
      pending.current = false
    }
  }
  return { busy, error, run, notifyNotice }
}

export function ResourceSaverSettings() {
  const status = usePolledResource(async (signal) => {
    const result = await apiGet<SaverStatus>('/api/runtime/resource-saver', signal)
    if (!result?.state?.config) throw new Error('Resource Saver is unavailable. Restart the matching Porto daemon.')
    return result
  }, 5000, [], 'runtime:resource-saver')
  const action = useResourceAction()
  const [prior, setPrior] = useState<SaverStatus | null>(null)
  const [draft, setDraft] = useState<SaverConfig | null>(null)
  const [overrides, setOverrides] = useState('{}')
  const [dirty, setDirty] = useState(false)
  if (status.data && status.data !== prior) {
    setPrior(status.data)
    if (!dirty) {
      setDraft(status.data.state.config)
      setOverrides(JSON.stringify(status.data.state.config.overrides ?? {}, null, 2))
    }
  }
  const state = status.data?.state
  return (
    <section className="integration platformSettings" aria-labelledby="resource-saver-title">
      <div className="hygieneIntro">
        <h2 id="resource-saver-title">Resource Saver</h2>
        <p>Off by default. Containers and every member of a Compose application must also opt in through their inspector or labels.</p>
        <p className="hintLine">Pause reduces process CPU but keeps its resident memory. Scale-to-zero stops tasks and releases their memory; disk data remains, process memory does not. Stateful mounts require separate consent.</p>
        <p className="hintLine">Only Porto HTTP/HTTPS URLs wake services. Raw TCP/UDP, active terminals, execs, established connections, multi-process workloads, and unmanaged dependencies are excluded.</p>
        <p className="hintLine">After daemon downtime, paused ownership requires manual resume. Manually paused containers are never adopted.</p>
      </div>
      <div className="hygieneControls">
        {(status.error || action.error) && <p className="errorLine" role="alert">{action.error || status.error}</p>}
        {state?.message && <p className="hintLine" role="status">{state.message}</p>}
        {draft && (
          <form className="platformForm" onSubmit={(event) => {
            event.preventDefault()
            void action.run(async () => {
              const result = await apiSend<SaverStatus>('/api/runtime/resource-saver', 'PUT', { ...draft, overrides: parseSaverOverrides(overrides) })
              setDirty(false)
              status.update(() => result)
              action.notifyNotice('resources', 'Resource Saver policy saved. Existing owned sleepers retain their wake listeners.')
            })
          }}>
            <label className="settingsField">
              <span>Automatic idle action</span>
              <select value={draft.mode} disabled={action.busy} onChange={(event) => {
                const value = event.target.value
                if (isSaverMode(value)) { setDraft({ ...draft, mode: value }); setDirty(true) }
              }}>
                {SAVER_MODES.map((mode) => <option key={mode} value={mode}>{mode}</option>)}
              </select>
            </label>
            {([
              ['idleSeconds', 'Idle timeout (seconds)', 1, 86400],
              ['wakeSeconds', 'Wake deadline (seconds)', 1, 1200],
              ['maxPending', 'Maximum retained requests', 1, 1024],
              ['maxBodyBytes', 'Queued body limit (bytes)', 1, 2147483648],
            ] as const).map(([key, label, min, max]) => (
              <label className="settingsField" key={key}><span>{label}</span>
                <input type="number" min={min} max={max} step={1} required disabled={action.busy} value={draft[key]} onChange={(event) => { setDraft({ ...draft, [key]: Number(event.target.value) }); setDirty(true) }} />
              </label>
            ))}
            <label className="toggleRow">
              <span><strong>Also suspend the idle engine</strong><small>Requires a fresh all-namespace inventory, idle BuildKit, no Kubernetes nodes, and no mounted native files.</small></span>
              <input type="checkbox" checked={draft.suspendEngine} disabled={action.busy} onChange={(event) => { setDraft({ ...draft, suspendEngine: event.target.checked }); setDirty(true) }} />
            </label>
            <label className="settingsField"><span>Excluded application/container names</span>
              <input type="text" value={(draft.exclusions ?? []).join(', ')} disabled={action.busy} placeholder="database-*, important-api" onChange={(event) => { setDraft({ ...draft, exclusions: event.target.value.split(',').map((name) => name.trim()).filter(Boolean) }); setDirty(true) }} />
            </label>
            <label className="settingsField"><span>Per-application overrides (JSON)</span>
              <textarea value={overrides} disabled={action.busy} spellCheck={false} rows={4} onChange={(event) => { setOverrides(event.target.value); setDirty(true) }} />
              <small>Keys are compose:project or container:full-container-ID. Use mode and idleSeconds.</small>
            </label>
            <div className="platformActions">
              <button type="submit" disabled={action.busy || !dirty}>Save Resource Saver policy</button>
              <button type="button" disabled={action.busy} onClick={status.reload}>Refresh Resource Saver</button>
            </div>
          </form>
        )}
        {state && <p role="status">{state.pending} active/retained request(s); {Object.keys(state.entries).length} owned sleeper(s); engine {state.engineToken ? 'suspended or transitioning' : 'not suspended by Resource Saver'}.</p>}
        {state && Object.values(state.entries).map((entry) => <p className={entry.error ? 'errorLine' : 'hintLine'} key={entry.application.id}>{entry.application.name}: {entry.phase}{entry.error ? ` / ${entry.error}` : ''}</p>)}
        {status.data?.applications?.filter((application) => !application.eligible).map((application) => <p className="hintLine" key={application.id}>{application.name}: {application.reason}</p>)}
      </div>
    </section>
  )
}

export function EngineResourceSettings() {
  const status = usePolledResource<EngineResourceStatus>((signal) => apiGet('/api/docker/engine/resources', signal), 15000, [])
  const action = useResourceAction()
  const [prior, setPrior] = useState<EngineResourceStatus | null>(null)
  const [draft, setDraft] = useState<EngineLimits | null>(null)
  const [dirty, setDirty] = useState(false)
  if (status.data && status.data !== prior) {
    setPrior(status.data)
    if (!dirty) setDraft(status.data.limits)
  }
  return (
    <section className="integration platformSettings" aria-labelledby="engine-resource-title">
      <div className="hygieneIntro">
        <h2 id="engine-resource-title">Engine resource budgets</h2>
        <p>CPU, memory, swap, and disk budgets apply to Porto's owned Lima VM, not to external/native containerd.</p>
        <p className="hintLine">Existing engines require an explicit idle restart. Disks can grow but never shrink. Zero CPU/memory/disk preserves current settings or provider defaults; blank swap leaves guest swap unmanaged.</p>
      </div>
      <div className="hygieneControls">
        {(status.error || action.error) && <p className="errorLine" role="alert">{action.error || status.error}</p>}
        {status.data?.message && <p className="hintLine">{status.data.message}</p>}
        {status.data?.restartRequired && <p role="status">Saved budgets are pending an engine restart.</p>}
        {draft && <form className="platformForm" onSubmit={(event) => {
          event.preventDefault()
          void action.run(async () => {
            const result = await apiSend<EngineResourceStatus>('/api/docker/engine/resources', 'PUT', draft)
            status.update(() => result)
            setDirty(false)
            action.notifyNotice('resources', 'Engine budgets saved. Apply them after stopping active workloads.')
          })
        }}>
          {([['cpus', 'Virtual CPUs', 128], ['memoryMiB', 'Memory (MiB)', 1048576], ['diskGiB', 'Disk (GiB)', 10240]] as const).map(([key, label, max]) => (
            <label className="settingsField" key={key}><span>{label}</span><input type="number" min={0} max={max} step={1} value={draft[key]} disabled={action.busy || !status.data?.supported} onChange={(event) => { setDraft({ ...draft, [key]: Number(event.target.value) }); setDirty(true) }} /></label>
          ))}
          <label className="settingsField"><span>Managed guest swap (MiB)</span><input type="number" min={0} max={65536} step={1} value={draft.swapMiB ?? ''} placeholder="Leave existing swap unchanged" disabled={action.busy || !status.data?.supported} onChange={(event) => { setDraft({ ...draft, swapMiB: event.target.value === '' ? null : Number(event.target.value) }); setDirty(true) }} /></label>
          <div className="platformActions">
            <button type="submit" disabled={action.busy || !dirty || !status.data?.supported}>Save engine budgets</button>
            <button type="button" disabled={action.busy || dirty || !status.data?.current || !status.data?.supported} onClick={() => {
              if (!window.confirm('Restart only the idle Porto-owned engine to apply saved budgets? Active or paused tasks and builds block this change. Existing disks are preserved and cannot shrink.')) return
              void action.run(async () => {
                await apiSend('/api/docker/engine/resources/apply', 'POST', { restartEngine: true })
                status.reload()
                action.notifyNotice('resources', 'Engine budgets applied.')
              })
            }}>Apply budgets to idle engine</button>
          </div>
        </form>}
      </div>
    </section>
  )
}

export function ContainerResourceSaver({ container, onChanged }: { container: DockerContainer; onChanged: () => void }) {
  const action = useResourceAction()
  const [mode, setMode] = useState(container.labels?.['dev.porto.resource-saver'] || 'exclude')
  const [stateful, setStateful] = useState(container.labels?.['dev.porto.resource-saver.stateful'] === 'true')
  return (
    <section className="drawerPanel" aria-labelledby="container-saver-title">
      <h3 id="container-saver-title">Resource Saver opt-in</h3>
      <p className="hintLine">The global policy must also be enabled. Every Compose member must opt in; only Porto HTTP/HTTPS routes can wake the application.</p>
      <form className="inspectorForm" onSubmit={(event) => {
        event.preventDefault()
        void action.run(async () => {
          await apiSend(`/api/docker/containers/${encodeURIComponent(container.id)}/resource-saver`, 'POST', { mode, stateful })
          onChanged()
          action.notifyNotice('resources', 'Container Resource Saver policy saved.')
        })
      }}>
        <label><span>Idle behavior</span><select value={mode} disabled={action.busy} onChange={(event) => setMode(event.target.value)}>
          <option value="exclude">Never suspend automatically</option><option value="pause">Pause; preserve process memory</option><option value="stop">Scale to zero; release process memory</option>
        </select></label>
        <label className="toggleRow"><span><strong>Allow mounted/stateful data</strong><small>Explicit consent. Scale-to-zero preserves disk contents but not process memory.</small></span><input type="checkbox" checked={stateful} disabled={action.busy} onChange={(event) => setStateful(event.target.checked)} /></label>
        <button type="submit" disabled={action.busy}>Save container idle policy</button>
        {action.error && <p className="errorLine" role="alert">{action.error}</p>}
      </form>
    </section>
  )
}

export function ResourceSaverActivity() {
  const status = usePolledResource<SaverStatus>((signal) => apiGet('/api/runtime/resource-saver', signal), 5000, [], 'runtime:resource-saver')
  return (
    <section className="activityResources" aria-label="Resource Saver activity">
      <h2>Resource Saver</h2>
      {status.error && <p className="errorLine" role="alert">{status.error}</p>}
      {status.data?.state?.message && <p className="hintLine">{status.data.state.message}</p>}
      <div className="logViewport activityViewport" role="log" aria-label="Backend idle and wake events">
        {(status.data?.state?.events ?? []).slice(-20).map((event, index) => <div className="logLine stdout" key={`${event.at}:${index}`}>
          <time dateTime={event.at}>{new Date(event.at).toLocaleTimeString([], { hour12: false })}</time><span className="streamLabel">{event.action}</span><span className="logMessage">{event.application} {event.message}</span>
        </div>)}
      </div>
    </section>
  )
}
