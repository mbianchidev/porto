import { useRef, useState } from 'react'
import { apiGet, apiSend, errorMessage } from '../api'
import { cleanupRunTitle, cleanupStepSummary, DOCKER_CLEANUP_WARNING } from '../dockerCleanup'
import { usePolledResource } from '../hooks'
import type { DockerCleanupRun, DockerCleanupState, DockerCleanupStep } from '../types'
import { useMessages } from '../useMessages'

function CleanupStepResult({ step, images = false }: { step: DockerCleanupStep; images?: boolean }) {
  return (
    <div>
      <dt>{images ? 'Unused image references' : 'BuildKit cache (builder / buildx, cleaned once)'}</dt>
      <dd>
        <span>{cleanupStepSummary(step, images)}</span>
        {step.error && <p className="errorLine">{step.error}</p>}
        {step.output && (
          <details>
            <summary>{images ? 'Image cleanup output' : 'BuildKit cleanup output'}</summary>
            <pre>{step.output}</pre>
          </details>
        )}
      </dd>
    </div>
  )
}

export function DockerCleanupSettings({
  enabled,
  savedEnabled,
  dockerEnabled,
  disabled,
  saving,
  onChange,
  onSave,
}: {
  enabled: boolean
  savedEnabled: boolean
  dockerEnabled: boolean
  disabled: boolean
  saving: boolean
  onChange: (enabled: boolean) => void
  onSave: () => Promise<void>
}) {
  const { notifyError, notifyNotice } = useMessages()
  const cleanup = usePolledResource<DockerCleanupState>(
    (signal) => apiGet('/api/docker/cleanup', signal),
    3000,
    [savedEnabled, dockerEnabled],
    'docker:cleanup:settings',
  )
  const [submitting, setSubmitting] = useState(false)
  const submittingRef = useRef(false)
  const [runError, setRunError] = useState('')
  const snapshot = cleanup.data
  const running = snapshot?.runs.some((run) => run.status === 'running') ?? false
  const runtimePaused = !dockerEnabled || snapshot?.dockerEnabled === false
  const runtimeAvailable = dockerEnabled && snapshot?.dockerEnabled === true
  const runDisabled = !snapshot || !runtimeAvailable || !!cleanup.error || submitting || running
  const scheduled = snapshot?.enabled ?? savedEnabled
  const latestRun = snapshot?.runs[0]

  async function runNow() {
    if (submittingRef.current || runDisabled) return
    if (!window.confirm(`Run runtime cleanup now? ${DOCKER_CLEANUP_WARNING}`)) return
    submittingRef.current = true
    setSubmitting(true)
    setRunError('')
    try {
      const run = await apiSend<DockerCleanupRun>('/api/docker/cleanup', 'POST')
      cleanup.update((current) => current ? {
        ...current,
        runs: [run, ...current.runs.filter((item) => item.id !== run.id)].slice(0, 10),
      } : current)
      notifyNotice('cleanup', 'Manual runtime cleanup accepted. It continues if you leave this page; the completed result will appear here and in Activity.')
    } catch (err) {
      const message = errorMessage(err, 'Unable to start runtime cleanup')
      setRunError(message)
      notifyError('cleanup', message)
    } finally {
      cleanup.reload()
      submittingRef.current = false
      setSubmitting(false)
    }
  }

  return (
    <section className="integration dockerCleanupSettings" aria-labelledby="docker-cleanup-title">
      <div className="hygieneIntro">
        <h2 id="docker-cleanup-title">Runtime cleanup</h2>
        <p id="docker-cleanup-scope">{DOCKER_CLEANUP_WARNING}</p>
        <p>
          Built into Porto and off by default. Opt in for a first automatic run seven days after
          enabling. While enabled, each completed manual or scheduled attempt sets the next automatic attempt
          seven days later. Unrelated settings saves do not reset the schedule.
        </p>
      </div>
      <div className="hygieneControls">
        <label className="toggleRow">
          <span><strong>Enable weekly runtime cleanup</strong><small>Opt-in. Save to apply; scheduling pauses while Docker is disabled.</small></span>
          <input
            type="checkbox"
            checked={enabled}
            disabled={disabled}
            aria-describedby="docker-cleanup-scope"
            onChange={(event) => onChange(event.target.checked)}
          />
        </label>
        {enabled !== savedEnabled && (
          <p className="hintLine" role="status">
            Weekly cleanup will be {enabled ? 'enabled' : 'disabled'} when you save. Other draft settings will also be saved.
          </p>
        )}
        <div className={`integrationStatus ${running ? 'running' : 'idle'}`} role="status">
          <strong>{!scheduled ? 'Weekly cleanup off' : runtimePaused ? 'Weekly cleanup paused' : 'Weekly cleanup enabled'}</strong>
          {scheduled && !runtimeAvailable && (
            <span>
              {runtimePaused
                ? 'Automatic cleanup is paused while Docker is disabled.'
                : cleanup.error
                  ? 'The next run time is unavailable until cleanup status can be refreshed.'
                  : 'Loading cleanup schedule…'}
            </span>
          )}
          {scheduled && runtimeAvailable && (
            <span>
              {running
                ? 'Cleanup is running. The next automatic attempt will be seven days after completion.'
                : snapshot?.nextRunAt
                  ? <>Next automatic attempt: <time dateTime={snapshot.nextRunAt}>{new Date(snapshot.nextRunAt).toLocaleString()}</time>.</>
                  : 'The next automatic run time has not been reported yet.'}
            </span>
          )}
          {latestRun && <span>{cleanupRunTitle(latestRun)}</span>}
          {submitting && <span>Submitting a manual cleanup request…</span>}
        </div>
        <div className="settingsActions">
          <button type="button" disabled={disabled} onClick={onSave}>{saving ? 'Saving…' : 'Save cleanup setting'}</button>
          <button className="destructiveAction" type="button" disabled={runDisabled} onClick={runNow}>
            {submitting ? 'Starting…' : running ? 'Cleanup running…' : 'Run now'}
          </button>
        </div>
        <p className="hintLine">
          Run now works with weekly scheduling off and does not enable it.
          {!dockerEnabled && ' Enable Docker above to run cleanup.'}
          {' '}Accepted runs continue if you leave Settings. Results also appear in Activity.
        </p>
        {runError && <p className="errorLine" role="alert">{runError}</p>}
        {cleanup.error && (
          <p className="errorLine" role="alert">
            Unable to load cleanup status: {cleanup.error}
            {snapshot && ' Retained results may be out of date.'}
          </p>
        )}
        <div className="settingsActions">
          <h3 id="docker-cleanup-history-title">Recent cleanup runs</h3>
          <button type="button" onClick={cleanup.reload} disabled={submitting}>Refresh cleanup status</button>
        </div>
        {!snapshot && cleanup.loading && <p role="status">Loading cleanup history…</p>}
        {snapshot?.runs.length === 0 && !cleanup.error && (
          <p className="hintLine">No cleanup runs yet. Manual and scheduled results will appear here; Porto keeps the latest 10 attempts.</p>
        )}
        {snapshot && snapshot.runs.length > 0 && (
          <ol className="cleanupRuns" aria-labelledby="docker-cleanup-history-title">
            {snapshot.runs.map((run) => (
              <li
                key={run.id}
                className={`integrationStatus cleanupRun ${run.status === 'succeeded' ? 'ready' : run.status === 'failed' || run.status === 'interrupted' ? 'error' : run.status === 'skipped' ? 'missing' : 'running'}`}
              >
                <h4>{cleanupRunTitle(run)}</h4>
                <span className="hintLine">
                  Started <time dateTime={run.startedAt}>{new Date(run.startedAt).toLocaleString()}</time>
                  {run.completedAt && <> · Completed <time dateTime={run.completedAt}>{new Date(run.completedAt).toLocaleString()}</time></>}
                </span>
                {run.error && <p className="errorLine">{run.error}</p>}
                <dl className="cleanupSteps">
                  <CleanupStepResult step={run.result.buildCache} />
                  <CleanupStepResult step={run.result.images} images />
                </dl>
              </li>
            ))}
          </ol>
        )}
      </div>
    </section>
  )
}
