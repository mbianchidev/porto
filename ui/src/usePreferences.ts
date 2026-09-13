import { useContext } from 'react'
import { PreferencesContext } from './preferencesContext'

export function usePreferences() {
  return useContext(PreferencesContext)
}
