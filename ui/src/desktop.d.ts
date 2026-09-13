import type { DesktopPreferences, DesktopUpdateStatus } from './types'

declare global {
  interface Window {
    portoDesktop?: {
      getPreferences: () => Promise<DesktopPreferences>
      setPreferences: (preferences: Pick<DesktopPreferences, 'openAtLogin' | 'keepInTray' | 'automaticallyDownloadUpdates'>) => Promise<DesktopPreferences>
      getUpdateStatus: () => Promise<DesktopUpdateStatus>
      checkForUpdates: () => Promise<DesktopUpdateStatus>
      downloadUpdate: () => Promise<DesktopUpdateStatus>
      restartAndInstallUpdate: () => Promise<DesktopUpdateStatus>
      onUpdateStatus: (listener: (status: DesktopUpdateStatus) => void) => () => void
    }
  }
}

export {}
