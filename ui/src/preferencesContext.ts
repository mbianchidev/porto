import { createContext } from 'react'
import {
  DEFAULT_EXPERIENCE_PREFERENCES,
  type ExperiencePreferences,
} from './experiencePreferences'

export const PreferencesContext = createContext<ExperiencePreferences>(DEFAULT_EXPERIENCE_PREFERENCES)
