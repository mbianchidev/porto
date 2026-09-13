import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import {
  DEFAULT_EXPERIENCE_PREFERENCES,
  normalizeExperiencePreferences,
  type ExperiencePreferences,
} from './experiencePreferences'
import { PreferencesContext } from './preferencesContext'
import type { Settings } from './types'

const EXPERIENCE_STORAGE_KEY = 'porto.experiencePreferences.v1'

function loadCachedPreferences(): ExperiencePreferences {
  try {
    const raw = window.localStorage.getItem(EXPERIENCE_STORAGE_KEY)
    if (!raw) return DEFAULT_EXPERIENCE_PREFERENCES
    const parsed = JSON.parse(raw) as Partial<ExperiencePreferences>
    return {
      interfaceDensity: parsed.interfaceDensity === 'comfortable' ? 'comfortable' : 'compact',
      reduceMotion: typeof parsed.reduceMotion === 'boolean' ? parsed.reduceMotion : DEFAULT_EXPERIENCE_PREFERENCES.reduceMotion,
      terminalFontSize: typeof parsed.terminalFontSize === 'number' && parsed.terminalFontSize >= 10 && parsed.terminalFontSize <= 24
        ? parsed.terminalFontSize
        : DEFAULT_EXPERIENCE_PREFERENCES.terminalFontSize,
      terminalLineHeight: typeof parsed.terminalLineHeight === 'number' && parsed.terminalLineHeight >= 1.1 && parsed.terminalLineHeight <= 2
        ? parsed.terminalLineHeight
        : DEFAULT_EXPERIENCE_PREFERENCES.terminalLineHeight,
      terminalCursorBlink: typeof parsed.terminalCursorBlink === 'boolean'
        ? parsed.terminalCursorBlink
        : DEFAULT_EXPERIENCE_PREFERENCES.terminalCursorBlink,
      terminalScrollback: typeof parsed.terminalScrollback === 'number' && parsed.terminalScrollback >= 1000 && parsed.terminalScrollback <= 50000
        ? parsed.terminalScrollback
        : DEFAULT_EXPERIENCE_PREFERENCES.terminalScrollback,
    }
  } catch (error) {
    console.error('Unable to load cached Porto experience preferences', error)
    return DEFAULT_EXPERIENCE_PREFERENCES
  }
}

export function PreferencesProvider({ settings, children }: { settings: Settings | null; children: ReactNode }) {
  const [cachedPreferences] = useState(loadCachedPreferences)
  const preferences = useMemo(
    () => settings ? normalizeExperiencePreferences(settings) : cachedPreferences,
    [cachedPreferences, settings],
  )

  useEffect(() => {
    document.documentElement.dataset.density = preferences.interfaceDensity
    document.documentElement.dataset.reduceMotion = preferences.reduceMotion ? 'true' : 'false'
    if (settings) {
      try {
        window.localStorage.setItem(EXPERIENCE_STORAGE_KEY, JSON.stringify(preferences))
      } catch (error) {
        console.error('Unable to cache Porto experience preferences', error)
      }
    }
  }, [preferences, settings])

  return <PreferencesContext.Provider value={preferences}>{children}</PreferencesContext.Provider>
}
