import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { MessagesContext } from './messagesContext'
import type { ActivityEntry, ActivityLevel } from './types'

const MAX_ACTIVITY_ENTRIES = 200
const ERROR_BANNER_MS = 7000
const NOTICE_BANNER_MS = 4500
const ACTIVITY_STORAGE_KEY = 'porto.activity.v1'
const ACTIVITY_LEVELS = new Set<ActivityLevel>(['info', 'notice', 'error'])

function isActivityEntry(value: unknown): value is ActivityEntry {
  if (!value || typeof value !== 'object') return false
  const entry = value as Partial<ActivityEntry>
  return typeof entry.id === 'number'
    && Number.isSafeInteger(entry.id)
    && typeof entry.level === 'string'
    && ACTIVITY_LEVELS.has(entry.level as ActivityLevel)
    && typeof entry.message === 'string'
    && entry.message !== ''
    && typeof entry.source === 'string'
    && entry.source !== ''
    && typeof entry.at === 'string'
    && !Number.isNaN(Date.parse(entry.at))
}

function loadActivity(): ActivityEntry[] {
  try {
    const stored = window.localStorage.getItem(ACTIVITY_STORAGE_KEY)
    if (!stored) return []
    const parsed: unknown = JSON.parse(stored)
    return Array.isArray(parsed) ? parsed.filter(isActivityEntry).slice(0, MAX_ACTIVITY_ENTRIES) : []
  } catch (error) {
    console.error('Unable to load Porto activity history', error)
    return []
  }
}

function saveActivity(entries: ActivityEntry[]) {
  try {
    window.localStorage.setItem(ACTIVITY_STORAGE_KEY, JSON.stringify(entries))
  } catch (error) {
    console.error('Unable to save Porto activity history', error)
  }
}

/**
 * Single source of truth for the app-wide error/notice banners and the Activity
 * page's client-side action/error log. Every page reports through notifyError or
 * notifyNotice instead of holding its own banner state, so the Activity page and
 * the shell banner always agree.
 */
export function MessagesProvider({ children }: { children: ReactNode }) {
  const [entries, setEntries] = useState<ActivityEntry[]>(loadActivity)
  const [errorBanner, setErrorBanner] = useState('')
  const [noticeBanner, setNoticeBanner] = useState('')
  const nextID = useRef(entries.reduce((highest, entry) => Math.max(highest, entry.id), 0) + 1)

  const record = useCallback((level: ActivityLevel, source: string, message: string) => {
    setEntries((current) => {
      const entry: ActivityEntry = { id: nextID.current++, level, source, message, at: new Date().toISOString() }
      return [entry, ...current].slice(0, MAX_ACTIVITY_ENTRIES)
    })
  }, [])

  const notifyError = useCallback((source: string, message: string) => {
    setErrorBanner(message)
    record('error', source, message)
  }, [record])

  const notifyNotice = useCallback((source: string, message: string) => {
    setNoticeBanner(message)
    record('notice', source, message)
  }, [record])

  const dismissError = useCallback(() => setErrorBanner(''), [])
  const dismissNotice = useCallback(() => setNoticeBanner(''), [])
  const clearActivity = useCallback(() => setEntries([]), [])

  useEffect(() => saveActivity(entries), [entries])

  useEffect(() => {
    if (!errorBanner) return
    const timer = window.setTimeout(() => setErrorBanner(''), ERROR_BANNER_MS)
    return () => window.clearTimeout(timer)
  }, [errorBanner])

  useEffect(() => {
    if (!noticeBanner) return
    const timer = window.setTimeout(() => setNoticeBanner(''), NOTICE_BANNER_MS)
    return () => window.clearTimeout(timer)
  }, [noticeBanner])

  const value = useMemo(
    () => ({ entries, clearActivity, errorBanner, noticeBanner, notifyError, notifyNotice, dismissError, dismissNotice }),
    [entries, clearActivity, errorBanner, noticeBanner, notifyError, notifyNotice, dismissError, dismissNotice],
  )

  return <MessagesContext.Provider value={value}>{children}</MessagesContext.Provider>
}
