import type { Settings } from './types'

export type ExperiencePreferences = Pick<
  Settings,
  | 'interfaceDensity'
  | 'reduceMotion'
  | 'terminalFontSize'
  | 'terminalLineHeight'
  | 'terminalCursorBlink'
  | 'terminalScrollback'
>

export const DEFAULT_EXPERIENCE_PREFERENCES: ExperiencePreferences = {
  interfaceDensity: 'compact',
  reduceMotion: false,
  terminalFontSize: 12,
  terminalLineHeight: 1.35,
  terminalCursorBlink: true,
  terminalScrollback: 5000,
}

export function normalizeExperiencePreferences(settings: Settings | null): ExperiencePreferences {
  return {
    interfaceDensity: settings?.interfaceDensity === 'comfortable' ? 'comfortable' : 'compact',
    reduceMotion: settings?.reduceMotion ?? DEFAULT_EXPERIENCE_PREFERENCES.reduceMotion,
    terminalFontSize: settings?.terminalFontSize || DEFAULT_EXPERIENCE_PREFERENCES.terminalFontSize,
    terminalLineHeight: settings?.terminalLineHeight || DEFAULT_EXPERIENCE_PREFERENCES.terminalLineHeight,
    terminalCursorBlink: settings?.terminalCursorBlink ?? DEFAULT_EXPERIENCE_PREFERENCES.terminalCursorBlink,
    terminalScrollback: settings?.terminalScrollback || DEFAULT_EXPERIENCE_PREFERENCES.terminalScrollback,
  }
}
