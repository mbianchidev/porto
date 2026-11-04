import { useEffect, useRef, useState } from 'react'
import { errorMessage } from '../api'
import { usePolledResource } from '../hooks'
import { useMessages } from '../useMessages'
import type { DesktopUpdatePhase, DesktopUpdateStatus } from '../types'

const BROWSER_UPDATE_STATUS: DesktopUpdateStatus = {
  phase: 'unsupported',
  currentVersion: '',
  availableVersion: '',
  progressPercent: null,
  releaseURL: '',
  message: 'Open Porto as the installed desktop app to check for updates.',
}

function updateStatusTone(phase: DesktopUpdatePhase) {
  if (phase === 'error') return 'error'
  if (phase === 'unsupported') return 'unsupported'
  if (['checking', 'available', 'downloading', 'installing'].includes(phase)) return 'checking'
  return 'ready'
}

export function SoftwareUpdateSettings() {
  const { notifyError } = useMessages()
  const bridge = window.portoDesktop
  const updates = usePolledResource<DesktopUpdateStatus>(
    () => bridge ? bridge.getUpdateStatus() : Promise.resolve(BROWSER_UPDATE_STATUS),
    0,
    [bridge],
  )
  const { update: setUpdateStatus } = updates
  const [actionName, setActionName] = useState('')
  const actionPending = useRef(false)

  useEffect(() => {
    if (!bridge) return
    return bridge.onUpdateStatus((status) => setUpdateStatus(() => status))
  }, [bridge, setUpdateStatus])

  const status = updates.data ?? BROWSER_UPDATE_STATUS
  const ready = bridge !== undefined && updates.data !== null && status.phase !== 'unsupported'
  const busy = actionName !== '' || ['checking', 'downloading', 'installing'].includes(status.phase)
  const failed = updates.error !== '' || status.phase === 'error'
  const message = updates.error || (updates.loading && updates.data === null
    ? 'Loading Porto update status.'
    : status.message)

  async function runUpdateAction(
    name: string,
    action: () => Promise<DesktopUpdateStatus>,
    fallbackMessage: string,
  ) {
    if (!ready || busy || actionPending.current) return
    actionPending.current = true
    setActionName(name)
    try {
      const next = await action()
      setUpdateStatus(() => next)
    } catch (error) {
      const message = errorMessage(error, fallbackMessage)
      setUpdateStatus((current) => ({ ...(current ?? BROWSER_UPDATE_STATUS), phase: 'error', message }))
      notifyError('settings', message)
    } finally {
      actionPending.current = false
      setActionName('')
    }
  }

  return (
    <section className="integration softwareUpdateSettings" aria-labelledby="software-updates-title">
      <div className="hygieneIntro">
        <h2 id="software-updates-title">Software updates</h2>
        <p>Check for new Porto releases. Downloads are verified before you choose when to restart and install.</p>
      </div>
      <div className="hygieneControls">
        <div
          className={`integrationStatus ${failed ? 'error' : updateStatusTone(status.phase)}`}
          role={failed ? 'alert' : 'status'}
          aria-atomic="true"
          id="software-update-status"
        >
          <strong>Update status</strong>
          <span>{message}</span>
        </div>
        <div className="settingsActions softwareUpdateActions">
          <button
            type="button"
            disabled={!ready || busy}
            aria-describedby="software-update-status"
            onClick={() => bridge && void runUpdateAction(
              'check',
              bridge.checkForUpdates,
              'Unable to check for Porto updates',
            )}
          >
            {status.phase === 'checking' || actionName === 'check' ? 'Checking...' : 'Check for updates'}
          </button>
          {updates.error && (
            <button type="button" onClick={updates.reload}>Retry update status</button>
          )}
          {status.phase === 'available' && (
            <button
              type="button"
              disabled={!ready || busy}
              onClick={() => bridge && void runUpdateAction(
                'download',
                bridge.downloadUpdate,
                'Unable to download the Porto update',
              )}
            >
              Download Porto {status.availableVersion}
            </button>
          )}
          {status.phase === 'downloaded' && (
            <button
              type="button"
              disabled={!ready || busy}
              onClick={() => bridge && void runUpdateAction(
                'restart',
                bridge.restartAndInstallUpdate,
                'Unable to restart Porto for the update',
              )}
            >
              Restart and update
            </button>
          )}
        </div>
      </div>
    </section>
  )
}
