try {
  const preferences = JSON.parse(localStorage.getItem('porto.experiencePreferences.v1') || '{}')
  document.documentElement.dataset.density = preferences.interfaceDensity === 'comfortable' ? 'comfortable' : 'compact'
  document.documentElement.dataset.reduceMotion = preferences.reduceMotion === true ? 'true' : 'false'
} catch (error) {
  console.error('Unable to apply cached Porto experience preferences', error)
}
