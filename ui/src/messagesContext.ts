import { createContext } from 'react'
import type { ActivityEntry } from './types'

export type MessagesContextValue = {
  entries: ActivityEntry[]
  clearActivity: () => void
  errorBanner: string
  noticeBanner: string
  notifyError: (source: string, message: string) => void
  notifyNotice: (source: string, message: string) => void
  dismissError: () => void
  dismissNotice: () => void
}

export const MessagesContext = createContext<MessagesContextValue | null>(null)
