const fs = require('node:fs')
const path = require('node:path')

const DEFAULT_DESKTOP_PREFERENCES = Object.freeze({
  openAtLogin: false,
  keepInTray: false,
})

function normalizeDesktopPreferences(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    return { ...DEFAULT_DESKTOP_PREFERENCES }
  }
  return {
    openAtLogin: value.openAtLogin === true,
    keepInTray: value.keepInTray === true,
  }
}

function validateDesktopPreferences(value) {
  if (!value || typeof value !== 'object' || Array.isArray(value)) {
    throw new Error('desktop preferences must be an object')
  }
  if (typeof value.openAtLogin !== 'boolean') {
    throw new Error('openAtLogin must be a boolean')
  }
  if (typeof value.keepInTray !== 'boolean') {
    throw new Error('keepInTray must be a boolean')
  }
  return {
    openAtLogin: value.openAtLogin,
    keepInTray: value.keepInTray,
  }
}

function loadDesktopPreferences(filePath, onError = () => {}) {
  try {
    return normalizeDesktopPreferences(JSON.parse(fs.readFileSync(filePath, 'utf8')))
  } catch (error) {
    if (error.code !== 'ENOENT') onError(error)
    return { ...DEFAULT_DESKTOP_PREFERENCES }
  }
}

function saveDesktopPreferences(filePath, preferences) {
  const normalized = validateDesktopPreferences(preferences)
  const directory = path.dirname(filePath)
  const temporaryPath = `${filePath}.${process.pid}.tmp`
  fs.mkdirSync(directory, { recursive: true, mode: 0o700 })
  try {
    fs.writeFileSync(temporaryPath, `${JSON.stringify(normalized)}\n`, { encoding: 'utf8', mode: 0o600 })
    fs.chmodSync(temporaryPath, 0o600)
    fs.renameSync(temporaryPath, filePath)
  } catch (error) {
    try {
      fs.rmSync(temporaryPath, { force: true })
    } catch (cleanupError) {
      onCleanupError(cleanupError)
    }
    throw new Error(`save desktop preferences: ${error.message}`, { cause: error })
  }
}

function onCleanupError(error) {
  console.error('Unable to remove staged desktop preferences', error)
}

function loginItemSupported(platform, isPackaged) {
  return isPackaged && (platform === 'darwin' || platform === 'win32')
}

function loginItemOptions(preferences, platform, executablePath) {
  const normalized = validateDesktopPreferences(preferences)
  if (platform === 'darwin') {
    return {
      openAtLogin: normalized.openAtLogin,
    }
  }
  if (platform === 'win32') {
    return {
      openAtLogin: normalized.openAtLogin,
      path: executablePath,
      args: normalized.keepInTray ? ['--hidden'] : [],
    }
  }
  return null
}

function shouldStartHidden(preferences, argv) {
  const normalized = validateDesktopPreferences(preferences)
  if (!normalized.keepInTray) return false
  return argv.includes('--hidden')
}

function shouldHideWindow(preferences, isQuitting) {
  return validateDesktopPreferences(preferences).keepInTray && !isQuitting
}

module.exports = {
  DEFAULT_DESKTOP_PREFERENCES,
  loadDesktopPreferences,
  loginItemOptions,
  loginItemSupported,
  normalizeDesktopPreferences,
  saveDesktopPreferences,
  shouldHideWindow,
  shouldStartHidden,
  validateDesktopPreferences,
}
