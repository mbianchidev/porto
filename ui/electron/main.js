// Secure Porto desktop shell.
//
// This process never talks to the daemon's data on its own: it only opens a
// window pointed at the local daemon's web UI, and starts the daemon when it
// finds it unreachable. contextIsolation stays on and nodeIntegration stays off so the
// loaded page runs like any other web page with no access to Node or desktop runtime
// internals; the preload script intentionally exposes nothing.
const { app, BrowserWindow, dialog, shell } = require('electron')
const { spawn } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')

const APP_NAME = 'Porto'
const APP_ID = 'dev.mbianchi.porto'
const APP_ICON = path.join(__dirname, 'assets', 'porto.png')
const DAEMON_URL = 'http://127.0.0.1:37623'
const HEALTH_CHECK_TIMEOUT_MS = 800
const windows = new Set()

app.setName(APP_NAME)
process.title = APP_NAME

async function isDaemonHealthy() {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), HEALTH_CHECK_TIMEOUT_MS)
  try {
    const response = await fetch(`${DAEMON_URL}/api/health`, { signal: controller.signal })
    if (!response.ok) return false
    const health = await response.json()
    return health.apiVersion === 1
  } catch {
    return false
  } finally {
    clearTimeout(timer)
  }
}

function portoBinary() {
  const binary = process.platform === 'win32' ? 'porto.exe' : 'porto'
  const candidates = [
    process.env.PORTO_BINARY,
    path.join(process.resourcesPath, binary),
    binary,
  ].filter(Boolean)
  return candidates.find((candidate) => candidate === binary || fs.existsSync(candidate)) || binary
}

// Starts `porto daemon start` detached from this process. The daemon manages
// its own lifecycle independently of the window: closing the Porto window
// must never stop it, so the child is fully detached and unref'd rather than
// tracked or killed on app quit.
function startDaemon() {
  return new Promise((resolve, reject) => {
    const child = spawn(portoBinary(), ['daemon', 'start'], {
      detached: true,
      stdio: 'ignore',
    })
    child.once('error', reject)
    child.once('spawn', () => {
      child.unref()
      resolve()
    })
  })
}

async function ensureDaemonRunning() {
  if (await isDaemonHealthy()) return true
  try {
    await startDaemon()
  } catch {
    return false
  }
  for (let attempt = 0; attempt < 30; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 300))
    if (await isDaemonHealthy()) return true
  }
  return false
}

function createWindow() {
  const window = new BrowserWindow({
    width: 1440,
    height: 900,
    minWidth: 960,
    minHeight: 600,
    backgroundColor: '#252925',
    title: APP_NAME,
    icon: APP_ICON,
    show: false,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      preload: path.join(__dirname, 'preload.js'),
    },
  })
  const presentWindow = () => {
    if (process.platform === 'darwin') app.dock?.show()
    app.focus({ steal: true })
    if (window.isMinimized()) window.restore()
    window.show()
    window.focus()
    window.moveTop()
  }
  let retries = 0
  const openExternalURL = (targetURL) => {
    try {
      const parsed = new URL(targetURL)
      if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
        shell.openExternal(parsed.toString())
      }
    } catch (error) {
      console.error('Refusing to open invalid external URL', error)
    }
  }
  window.webContents.on('will-navigate', (event, targetURL) => {
    if (new URL(targetURL).origin !== DAEMON_URL) {
      event.preventDefault()
      openExternalURL(targetURL)
    }
  })
  window.webContents.setWindowOpenHandler(({ url }) => {
    openExternalURL(url)
    return { action: 'deny' }
  })
  window.webContents.on('did-fail-load', (_event, errorCode) => {
    if (errorCode === -3 || retries >= 10) return
    retries += 1
    setTimeout(() => window.loadURL(DAEMON_URL), 500)
  })
  windows.add(window)
  window.on('closed', () => windows.delete(window))
  presentWindow()
  window.once('ready-to-show', presentWindow)
  window.loadURL(DAEMON_URL).then(() => {
    presentWindow()
  }).catch((error) => {
    console.error('Unable to load Porto dashboard', error)
  })
}

const hasLock = app.requestSingleInstanceLock()
if (!hasLock) {
  app.quit()
}

app.on('second-instance', () => {
  const window = BrowserWindow.getAllWindows()[0]
  if (!window) {
    createWindow()
    return
  }
  if (window.isMinimized()) window.restore()
  window.focus()
})

app.whenReady().then(async () => {
  app.setAppUserModelId(APP_ID)
  if (process.platform === 'darwin') app.dock?.setIcon(APP_ICON)
  const healthy = await ensureDaemonRunning()
  if (!healthy) {
    dialog.showErrorBox(
      'Porto daemon unavailable',
      `Could not start ${portoBinary()} daemon start. Set PORTO_BINARY or install porto on PATH, then retry.`,
    )
    app.quit()
    return
  }
  createWindow()

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', () => {
  // Intentionally does not stop the Porto daemon: it keeps managing projects,
  // containers, clusters, and VMs regardless of whether the window is open.
  if (process.platform !== 'darwin') app.quit()
})
