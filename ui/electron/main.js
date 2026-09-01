// Secure Porto desktop shell.
//
// This process never talks to the daemon's data on its own: it only opens a
// window pointed at the local daemon's web UI, and starts the daemon when it
// finds it unreachable. contextIsolation stays on and nodeIntegration stays off so the
// loaded page runs like any other web page with no access to Node or desktop runtime
// internals; the preload script intentionally exposes nothing.
const { app, BrowserWindow, dialog, shell } = require('electron')
const { execFile, spawn } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')
const { promisify } = require('node:util')

const {
  daemonProcessIDs,
  dockerBootstrapCommand,
  inspectDaemon,
  inspectDockerStatus,
  installDockerEngine,
  windowsDaemonProcessIDs,
} = require('./daemon-readiness.cjs')

const APP_NAME = 'Porto'
const APP_ID = 'dev.mbianchi.porto'
const APP_ICON = path.join(__dirname, 'assets', 'porto.png')
const DAEMON_URL = 'http://127.0.0.1:37623'
const windows = new Set()
const execFileAsync = promisify(execFile)

app.setName(APP_NAME)
process.title = APP_NAME

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
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

function portoEnvironment() {
  const pathKey = Object.keys(process.env).find((key) => key.toLowerCase() === 'path') || 'PATH'
  const environment = { ...process.env }
  for (const key of Object.keys(environment)) {
    if (key.toLowerCase() === 'docker_host' || key.toLowerCase() === 'docker_context') {
      delete environment[key]
    }
  }
  const bundledPaths = [
    path.join(process.resourcesPath, 'runtime', 'bin'),
    path.join(process.resourcesPath, 'runtime', 'lima', 'bin'),
  ].filter((candidate) => fs.existsSync(candidate))
  if (bundledPaths.length > 0) {
    environment[pathKey] = [...bundledPaths, process.env[pathKey]].filter(Boolean).join(path.delimiter)
  }
  return environment
}

// Starts `porto daemon start` detached from this process. The daemon manages
// its own lifecycle independently of the window: closing the Porto window
// must never stop it, so the child is fully detached and unref'd rather than
// tracked or killed on app quit.
function startDaemon() {
  return new Promise((resolve, reject) => {
    const child = spawn(portoBinary(), ['daemon', 'start'], {
      detached: true,
      env: portoEnvironment(),
      stdio: 'ignore',
    })
    child.once('error', reject)
    child.once('spawn', () => {
      child.unref()
      resolve()
    })
  })
}

async function runningDaemonPIDs() {
  if (process.platform === 'win32') {
    const script = 'Get-CimInstance Win32_Process | Select-Object ProcessId,CommandLine | ConvertTo-Json -Compress'
    const { stdout } = await execFileAsync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
      timeout: 10000,
      maxBuffer: 4 * 1024 * 1024,
    })
    if (stdout.trim() === '') return []
    return windowsDaemonProcessIDs(JSON.parse(stdout))
  }
  const { stdout } = await execFileAsync('ps', ['-ax', '-o', 'pid=,command='], {
    timeout: 5000,
    maxBuffer: 1024 * 1024,
  })
  return daemonProcessIDs(stdout)
}

async function stopDaemonPIDs(pids) {
  const runningPIDs = new Set(pids)
  for (const pid of pids) {
    try {
      process.kill(pid, 'SIGTERM')
    } catch (error) {
      if (error.code === 'ESRCH') {
        runningPIDs.delete(pid)
        continue
      }
      console.error(`Unable to stop incompatible Porto daemon ${pid}`, error)
      return false
    }
  }
  for (let attempt = 0; attempt < 240; attempt += 1) {
    await delay(250)
    for (const pid of runningPIDs) {
      try {
        process.kill(pid, 0)
      } catch (error) {
        if (error.code === 'ESRCH') runningPIDs.delete(pid)
      }
    }
    if (runningPIDs.size === 0 && !(await inspectDaemon({ daemonURL: DAEMON_URL })).reachable) {
      return true
    }
  }
  return false
}

async function ensureDaemonRunning() {
  let existing = await inspectDaemon({ daemonURL: DAEMON_URL })
  if (existing.ready) return true
  let pids
  try {
    pids = await runningDaemonPIDs()
  } catch (error) {
    console.error('Unable to inspect existing Porto daemons', error)
    return false
  }
  if (pids.length > 0) {
    if (!existing.reachable) {
      for (let attempt = 0; attempt < 10; attempt += 1) {
        await delay(300)
        existing = await inspectDaemon({ daemonURL: DAEMON_URL })
        if (existing.ready) return true
        if (existing.reachable) break
      }
    }
    if (!(await stopDaemonPIDs(pids))) return false
  } else if (existing.reachable) {
    return false
  }
  for (let startAttempt = 0; startAttempt < 3; startAttempt += 1) {
    try {
      await startDaemon()
    } catch {
      continue
    }
    for (let attempt = 0; attempt < 30; attempt += 1) {
      await delay(300)
      if ((await inspectDaemon({ daemonURL: DAEMON_URL })).ready) return true
    }
  }
  return false
}

async function ensureDockerEngine() {
  let status = await inspectDockerStatus({ daemonURL: DAEMON_URL })
  const command = dockerBootstrapCommand(status, {
    isPackaged: app.isPackaged,
    platform: process.platform,
  })
  if (command === null) return
  await installDockerEngine({ daemonURL: DAEMON_URL })
  for (let attempt = 0; attempt < 30; attempt += 1) {
    status = await inspectDockerStatus({ daemonURL: DAEMON_URL })
    if (status.available) return
    await delay(500)
  }
  throw new Error(status.message || 'Porto container runtime did not become available')
}

function createBootstrapWindow() {
  const window = new BrowserWindow({
    width: 460,
    height: 210,
    resizable: false,
    backgroundColor: '#252925',
    title: APP_NAME,
    icon: APP_ICON,
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
    },
  })
  const html = `<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Preparing Porto</title>
    <style>
      body { margin: 0; background: #252925; color: #dedfd7; font: 14px system-ui, sans-serif; }
      main { padding: 38px; }
      h1 { margin: 0 0 12px; color: #fffdf7; font-size: 18px; letter-spacing: .08em; text-transform: uppercase; }
      p { margin: 0; color: #d5d9b8; line-height: 1.5; }
    </style>
  </head>
  <body><main><h1>Preparing Porto</h1><p>Starting the daemon and bundled container runtime. First launch can take a few minutes.</p></main></body>
</html>`
  window.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(html)}`)
  return window
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
  const bootstrapWindow = createBootstrapWindow()
  const healthy = await ensureDaemonRunning()
  if (!healthy) {
    bootstrapWindow.close()
    dialog.showErrorBox(
      'Porto daemon unavailable',
      `Could not start ${portoBinary()} daemon start. Another Porto daemon may be incompatible or missing its dashboard; stop it and retry.`,
    )
    app.quit()
    return
  }
  let dockerError = null
  try {
    await ensureDockerEngine()
  } catch (error) {
    dockerError = error
    console.error('Unable to prepare the bundled Porto container runtime', error)
  }
  createWindow()
  bootstrapWindow.close()
  if (dockerError !== null) {
    dialog.showErrorBox('Porto container runtime unavailable', dockerError.message)
  }

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', () => {
  // Intentionally does not stop the Porto daemon: it keeps managing projects,
  // containers, clusters, and VMs regardless of whether the window is open.
  if (process.platform !== 'darwin') app.quit()
})
