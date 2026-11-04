import { useEffect, useState } from 'react'
import { errorMessage } from '../api'
import { useMessages } from '../useMessages'
import type { DesktopPreferences } from '../types'

const BROWSER_DEFAULTS: DesktopPreferences = {
  openAtLogin: false,
  keepInTray: false,
  automaticallyDownloadUpdates: false,
  loginItemSupported: false,
}

export function DesktopBehaviorSettings() {
  const { notifyError, notifyNotice } = useMessages()
  const bridge = window.portoDesktop
  const [preferences, setPreferences] = useState<DesktopPreferences | null>(
    bridge ? null : BROWSER_DEFAULTS,
  )
  const [loadError, setLoadError] = useState('')
  const [saving, setSaving] = useState(false)

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

  const current = preferences ?? BROWSER_DEFAULTS
  const desktopAvailable = bridge !== undefined
  const loading = desktopAvailable && preferences === null && loadError === ''
  const desktopReady = desktopAvailable && loadError === '' && preferences !== null
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
        <h2 id="desktop-behavior-title">Desktop behavior</h2>
        <p>Control desktop startup, background behavior, and automatic update downloads.</p>
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
      </div>
    </section>
  )
}
