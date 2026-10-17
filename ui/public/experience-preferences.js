try {
  const preferences = JSON.parse(localStorage.getItem('porto.experiencePreferences.v1') || '{}')
  document.documentElement.dataset.density = preferences.interfaceDensity === 'comfortable' ? 'comfortable' : 'compact'
  const theme = preferences.theme === 'light' || preferences.theme === 'system' ? preferences.theme : 'dark'
  const prefersLight = theme === 'system' && typeof matchMedia === 'function' && matchMedia('(prefers-color-scheme: light)').matches
  document.documentElement.dataset.theme = theme === 'light' || prefersLight ? 'light' : 'dark'
  document.documentElement.dataset.reduceMotion = preferences.reduceMotion === true ? 'true' : 'false'
} catch (error) {
  document.documentElement.dataset.theme = 'dark'
  console.error('Unable to apply cached Porto experience preferences', error)
}
