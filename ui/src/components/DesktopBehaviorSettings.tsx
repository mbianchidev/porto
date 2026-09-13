import { useEffect, useState } from 'react'
import { errorMessage } from '../api'
import { useMessages } from '../useMessages'
import type { DesktopPreferences } from '../types'

const BROWSER_DEFAULTS: DesktopPreferences = {
  openAtLogin: false,
  keepInTray: false,
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
      })
      setPreferences(saved)
      setLoadError('')
      notifyNotice('settings', 'Desktop behavior saved.')
    } catch (error) {
      notifyError('settings', errorMessage(error, 'Unable to save desktop behavior'))
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
        <h2 id="desktop-behavior-title">Keep Porto close at hand.</h2>
        <p>Launch it with your desktop session, or let the control desk keep working after its window closes.</p>
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
        <button type="button" onClick={save} disabled={!desktopReady || saving}>
          {saving ? 'Saving…' : 'Save desktop behavior'}
        </button>
      </div>
    </section>
  )
}
