import { useEffect, useState } from 'react'
import { errorMessage } from '../api'
import { useMessages } from '../useMessages'
import type { DesktopPreferences, DesktopUpdatePhase, DesktopUpdateStatus } from '../types'

const BROWSER_DEFAULTS: DesktopPreferences = {
  openAtLogin: false,
  keepInTray: false,
  automaticallyDownloadUpdates: false,
  loginItemSupported: false,
}

const BROWSER_UPDATE_STATUS: DesktopUpdateStatus = {
  phase: 'unsupported',
  currentVersion: '',
  availableVersion: '',
  progressPercent: null,
  releaseURL: '',
  message: 'Open Porto as the installed desktop app to check for updates.',
}

function updateStatusTone(phase: DesktopUpdatePhase) {
  if (phase === 'downloaded' || phase === 'up-to-date') return 'ready'
  if (phase === 'error') return 'error'
  if (phase === 'unsupported') return 'unsupported'
  if (phase === 'checking' || phase === 'available' || phase === 'downloading' || phase === 'installing') {
    return 'checking'
  }
  return 'ready'
}

export function DesktopBehaviorSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const bridge = window.portoDesktop
  const [preferences, setPreferences] = useState<DesktopPreferences | null>(
    bridge ? null : BROWSER_DEFAULTS,
  )
  const [loadError, setLoadError] = useState('')
  const [saving, setSaving] = useState(false)
  const [updateStatus, setUpdateStatus] = useState<DesktopUpdateStatus | null>(
    bridge ? null : BROWSER_UPDATE_STATUS,
  )
  const [updateAction, setUpdateAction] = useState('')

  useEffect(() => {
    if (!bridge) return

    let active = true
    void bridge.getPreferences()
      .then((loaded) => {
        if (active) setPreferences(loaded)
      })
      .catch((error) => {
        if (active) {
          setPreferences(BROWSER_DEFAULTS)
          setLoadError(errorMessage(error, 'Unable to load desktop behavior settings'))
        }
      })
    return () => {
      active = false
    }
  }, [bridge])

  useEffect(() => {
    if (!bridge) return

    let active = true
    const unsubscribe = bridge.onUpdateStatus((status) => {
      if (active) setUpdateStatus(status)
    })
    void bridge.getUpdateStatus()
      .then((status) => {
        if (active) setUpdateStatus(status)
      })
      .catch((error) => {
        if (active) {
          setUpdateStatus({
            ...BROWSER_UPDATE_STATUS,
            phase: 'error',
            message: errorMessage(error, 'Unable to load Porto update status'),
          })
        }
      })
    return () => {
      active = false
      unsubscribe()
    }
  }, [bridge])

  async function save() {
    if (!bridge || !preferences) return
    setSaving(true)
    try {
      const saved = await bridge.setPreferences({
        openAtLogin: preferences.openAtLogin,
        keepInTray: preferences.keepInTray,
        automaticallyDownloadUpdates: preferences.automaticallyDownloadUpdates,
      })
      setPreferences(saved)
      setLoadError('')
      notifyNotice('settings', 'Desktop preferences saved.')
    } catch (error) {
      notifyError('settings', errorMessage(error, 'Unable to save desktop preferences'))
    } finally {
      setSaving(false)
    }
  }

  async function runUpdateAction(
    actionName: string,
    action: () => Promise<DesktopUpdateStatus>,
    fallbackMessage: string,
  ) {
    setUpdateAction(actionName)
    try {
      setUpdateStatus(await action())
    } catch (error) {
      notifyError('settings', errorMessage(error, fallbackMessage))
    } finally {
      setUpdateAction('')
    }
  }

  const current = preferences ?? BROWSER_DEFAULTS
  const currentUpdate = updateStatus ?? BROWSER_UPDATE_STATUS
  const desktopAvailable = bridge !== undefined
  const loading = desktopAvailable && preferences === null && loadError === ''
  const desktopReady = desktopAvailable && loadError === '' && preferences !== null
  const updateReady = desktopAvailable && updateStatus !== null
  const updateBusy = updateAction !== '' ||
    currentUpdate.phase === 'checking' ||
    currentUpdate.phase === 'downloading' ||
    currentUpdate.phase === 'installing'
  const state = loadError ? 'error' : loading ? 'checking' : desktopAvailable ? 'ready' : 'unsupported'
  const message = loadError ||
    (loading
      ? 'Loading desktop behavior settings.'
      : desktopAvailable
      ? current.loginItemSupported
        ? 'Desktop controls are connected. Changes take effect immediately.'
        : 'Tray behavior is available. Open at login requires an installed macOS or Windows build.'
      : 'Open Porto as the installed desktop app to configure login and tray behavior.')

  return (
    <section className="integration desktopBehaviorSettings" aria-labelledby="desktop-behavior-title">
      <div className="hygieneIntro">
        <h2 id="desktop-behavior-title">Keep Porto close and current.</h2>
        <p>Control desktop startup, background behavior, and verified updates from GitHub Releases.</p>
      </div>
      <div className="hygieneControls">
        <div className={`integrationStatus ${state}`} role={loadError ? 'alert' : 'status'}>
          <strong>{desktopAvailable ? 'desktop shell' : 'desktop app only'}</strong>
          <span>{message}</span>
        </div>
        <label className="toggleRow">
          <span><strong>Open Porto when I sign in</strong><small>Windows starts quietly in the tray when close-to-tray is also enabled.</small></span>
          <input
            type="checkbox"
            checked={current.openAtLogin}
            disabled={!desktopReady || !current.loginItemSupported || saving}
            onChange={(event) => setPreferences({ ...current, openAtLogin: event.target.checked })}
          />
        </label>
        <label className="toggleRow">
          <span><strong>Close the window to the menu bar or system tray</strong><small>The tray menu always provides Open Porto and Quit Porto.</small></span>
          <input
            type="checkbox"
            checked={current.keepInTray}
            disabled={!desktopReady || saving}
            onChange={(event) => setPreferences({ ...current, keepInTray: event.target.checked })}
          />
        </label>
        <label className="toggleRow">
          <span><strong>Automatically download new Porto versions</strong><small>Porto verifies the release checksum, then waits for you to restart and install it.</small></span>
          <input
            type="checkbox"
            checked={current.automaticallyDownloadUpdates}
            disabled={!desktopReady || saving}
            onChange={(event) => setPreferences({ ...current, automaticallyDownloadUpdates: event.target.checked })}
          />
        </label>
        <button type="button" onClick={save} disabled={!desktopReady || saving}>
          {saving ? 'Saving…' : 'Save desktop preferences'}
        </button>
        <div
          className={`integrationStatus ${updateStatusTone(currentUpdate.phase)}`}
          role={currentUpdate.phase === 'error' ? 'alert' : 'status'}
          aria-atomic="true"
        >
          <strong>software updates</strong>
          <span>{updateStatus === null ? 'Loading Porto update status.' : currentUpdate.message}</span>
        </div>
        <div className="settingsActions">
          <button
            type="button"
            disabled={!updateReady || updateBusy}
            onClick={() => bridge && void runUpdateAction(
              'check',
              bridge.checkForUpdates,
              'Unable to check for Porto updates',
            )}
          >
            {currentUpdate.phase === 'checking' || updateAction === 'check' ? 'Checking…' : 'Check for updates'}
          </button>
          {currentUpdate.phase === 'available' && (
            <button
              type="button"
              disabled={!updateReady || updateBusy}
              onClick={() => bridge && void runUpdateAction(
                'download',
                bridge.downloadUpdate,
                'Unable to download the Porto update',
              )}
            >
              Download Porto {currentUpdate.availableVersion}
            </button>
          )}
          {currentUpdate.phase === 'downloaded' && (
            <button
              type="button"
              disabled={!updateReady || updateBusy}
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
