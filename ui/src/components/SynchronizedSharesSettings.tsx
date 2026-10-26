import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { Project } from '../types'

type SyncProblem = { path: string; error: string }
type SyncEndpoint = {
  connected: boolean
  scanned: boolean
  files?: number
  scanProblems?: SyncProblem[]
  transitionProblems?: SyncProblem[]
  stagingProgress?: unknown
}
type Share = {
  id: string
  projectId: number
  hostPath: string
  guestPath: string
  mode: 'bind' | 'synchronized'
  status: string
  ignore: string[]
  error?: string
  session?: { paused: boolean; status: string; lastError?: string; conflicts?: unknown[]; alpha: SyncEndpoint; beta: SyncEndpoint }
}
type ShareStatus = { supported: boolean; version?: string; message?: string; shares: Share[] }

export function SynchronizedSharesSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const status = usePolledResource(async (signal) => {
    const result = await apiGet<ShareStatus>('/api/runtime/shares', signal)
    if (!result || !Array.isArray(result.shares)) throw new Error('Synchronized sharing is unavailable. Restart the matching Porto daemon.')
    return result
  }, 5000, [], 'runtime:shares')
  const projects = usePolledResource<Project[]>((signal) => apiGet('/api/projects', signal), 15000, [])
  const [projectID, setProjectID] = useState('')
  const [ignores, setIgnores] = useState('node_modules')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')
  const pending = useRef(false)
  async function perform(action: () => Promise<void>) {
    if (pending.current) return
    pending.current = true
    setBusy(true)
    setFailure('')
    try {
      await action()
    } catch (error) {
      const message = errorMessage(error, 'Synchronized sharing failed')
      setFailure(message)
      notifyError('shares', message)
    } finally {
      pending.current = false
      setBusy(false)
      status.reload()
    }
  }
  const shares = status.data?.shares ?? []
  return (
    <section className="integration platformSettings" aria-labelledby="synchronized-shares-title">
      <div className="hygieneIntro">
        <h2 id="synchronized-shares-title">Optimized project shares</h2>
        <p>Opt an explicitly selected project into bidirectional, two-way-safe synchronization with the owned Lima guest. Ordinary bind mounts remain available.</p>
        <p className="hintLine">Conflicting host and guest edits are reported, never resolved by silently preferring one side. VCS directories and selected ignore patterns stay local.</p>
        <p className="hintLine">Portable permissions preserve executable bits, not host ownership or ACLs. Relative contained symlinks are supported; unsafe links and case collisions appear as problems.</p>
        <p className="hintLine">Recreate containers after changing a share's binding mode. Paused or detached sessions preserve both copies. Active synchronization prevents engine suspension.</p>
      </div>
      <div className="hygieneControls">
        {(failure || status.error) && <p className="errorLine" role="alert">{failure || status.error}</p>}
        {status.data?.message && <p className="hintLine">{status.data.message}</p>}
        <form className="platformForm" onSubmit={(event) => {
          event.preventDefault()
          if (!projectID) return
          void perform(async () => {
            await apiSend('/api/runtime/shares', 'POST', { projectId: Number(projectID), ignore: ignores.split(',').map((pattern) => pattern.trim()).filter(Boolean) })
            notifyNotice('shares', 'Selected project synchronization is ready. New bind mounts inside its root use the owned guest copy.')
          })
        }}>
          <label className="settingsField"><span>Registered project root</span>
            <select value={projectID} required disabled={busy || !status.data?.supported} onChange={(event) => setProjectID(event.target.value)}>
              <option value="">Choose one project</option>
              {(projects.data ?? []).map((project) => <option key={project.id} value={project.id}>{project.name} / {project.path}</option>)}
            </select>
          </label>
          <label className="settingsField"><span>Additional ignore patterns</span><input type="text" value={ignores} disabled={busy} onChange={(event) => setIgnores(event.target.value)} /></label>
          <div className="platformActions">
            <button type="submit" disabled={busy || !status.data?.supported || !projectID}>{busy ? 'Preparing selected share...' : 'Enable selected project share'}</button>
            <button type="button" onClick={status.reload}>Refresh share progress</button>
          </div>
        </form>
        {projects.error && <p className="errorLine">{projects.error}</p>}
        {shares.map((share) => {
          const problems = [...(share.session?.alpha.scanProblems ?? []), ...(share.session?.beta.scanProblems ?? []),
            ...(share.session?.alpha.transitionProblems ?? []), ...(share.session?.beta.transitionProblems ?? [])]
          return (
            <article className="activityResourceGroup" key={share.id}>
              <header><strong>{share.hostPath}</strong><span role="status">{share.session?.status || share.status}</span></header>
              <p className="hintLine">Guest copy: <code>{share.guestPath}</code></p>
              <p className="hintLine">Binding mode: {share.mode}; host {share.session?.alpha.connected ? 'connected' : 'disconnected'}; guest {share.session?.beta.connected ? 'connected' : 'disconnected'}; {share.session?.alpha.files ?? 0} host file(s), {share.session?.beta.files ?? 0} guest file(s).</p>
              {share.error && <p className="errorLine" role="alert">{share.error}</p>}
              {!!share.session?.conflicts?.length && <p className="errorLine" role="alert">{share.session.conflicts.length} unresolved conflict(s). Inspect both filesystem copies before manually resolving them.</p>}
              {problems.map((problem, index) => <p className="errorLine" key={`${problem.path}:${index}`}>{problem.path}: {problem.error}</p>)}
              {!!share.session?.conflicts?.length && <details><summary>Conflict details</summary><pre>{JSON.stringify(share.session.conflicts, null, 2)}</pre></details>}
              <div className="platformActions">
                <button type="button" disabled={busy} onClick={() => void perform(async () => {
                  const mode = share.mode === 'synchronized' ? 'bind' : 'synchronized'
                  await apiSend(`/api/runtime/shares/${share.id}`, 'PUT', { mode })
                  notifyNotice('shares', `Selected share uses ${mode} mode for newly created containers.`)
                })}>{share.mode === 'synchronized' ? 'Pause sync; use ordinary binds' : 'Resume synchronized mode'}</button>
                <button type="button" className="destructiveAction" disabled={busy} onClick={() => {
                  if (!window.confirm(`Detach only the synchronization session for ${share.hostPath}? Host and guest copies, including unsynchronized edits, remain. Stop containers using the guest copy first.`)) return
                  void perform(async () => {
                    await apiSend(`/api/runtime/shares/${share.id}?confirm=true`, 'DELETE')
                    notifyNotice('shares', 'Selected synchronization session detached. Both filesystem copies were retained.')
                  })
                }}>Detach; keep both copies</button>
              </div>
            </article>
          )
        })}
      </div>
    </section>
  )
}
