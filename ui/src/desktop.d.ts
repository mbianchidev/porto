import type { DesktopPreferences } from './types'

declare global {
  interface Window {
    portoDesktop?: {
      getPreferences: () => Promise<DesktopPreferences>
      setPreferences: (preferences: Pick<DesktopPreferences, 'openAtLogin' | 'keepInTray'>) => Promise<DesktopPreferences>
    }
  }
}

export {}
