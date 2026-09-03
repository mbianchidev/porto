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
  daemonProcesses,
  dashboardLoadAction,
  dockerBootstrapCommand,
  inspectDaemon,
  inspectDockerStatus,
  installDockerContext,
  installDockerEngine,
  mergeExecutablePaths,
  resolvePackagedDashboard,
  resolvePortoBinary,
  resolveLoginShellPath,
  windowsDaemonProcesses,
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
  return resolvePortoBinary({
    isPackaged: app.isPackaged,
    platform: process.platform,
    resourcesPath: process.resourcesPath,
    environment: process.env,
    existsImpl: fs.existsSync,
  })
}

function bundledPortoBinaryReady() {
  if (!app.isPackaged) return true
  try {
    const mode = process.platform === 'win32' ? fs.constants.F_OK : fs.constants.F_OK | fs.constants.X_OK
    fs.accessSync(portoBinary(), mode)
    return true
  } catch {
    return false
  }
}

async function portoEnvironment() {
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
  const loginShellPath = await resolveLoginShellPath({ environment })
  environment[pathKey] = mergeExecutablePaths(
    [...bundledPaths, loginShellPath, process.env[pathKey]],
    path.delimiter,
  )
  const dashboard = resolvePackagedDashboard({
    isPackaged: app.isPackaged,
    resourcesPath: process.resourcesPath,
    existsImpl: fs.existsSync,
  })
  if (dashboard !== '') environment.PORTO_UI_DIR = dashboard
  return environment
}

function normalizedExecutablePath(value) {
  let resolved
  try {
    resolved = fs.realpathSync(value)
  } catch {
    resolved = path.resolve(value)
  }
  return process.platform === 'win32' ? resolved.toLocaleLowerCase() : resolved
}

// Starts `porto daemon start` detached from this process. The daemon manages
// its own lifecycle independently of the window: closing the Porto window
// must never stop it, so the child is fully detached and unref'd rather than
// tracked or killed on app quit.
async function startDaemon() {
  const environment = await portoEnvironment()
  return new Promise((resolve, reject) => {
    const child = spawn(portoBinary(), ['daemon', 'start'], {
      detached: true,
      env: environment,
      stdio: 'ignore',
    })
    child.once('error', reject)
    child.once('spawn', () => {
      child.unref()
      resolve()
    })
  })
}

async function runningDaemonProcesses() {
  let processes
  if (process.platform === 'win32') {
    const script = 'Get-CimInstance Win32_Process | Select-Object ProcessId,CommandLine | ConvertTo-Json -Compress'
    const { stdout } = await execFileAsync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
      timeout: 10000,
      maxBuffer: 4 * 1024 * 1024,
    })
    if (stdout.trim() === '') return []
    processes = windowsDaemonProcesses(JSON.parse(stdout))
  } else {
    const { stdout } = await execFileAsync('ps', ['-ax', '-o', 'pid=,command='], {
      timeout: 5000,
      maxBuffer: 1024 * 1024,
    })
    processes = daemonProcesses(stdout)
  }
  const identified = await Promise.all(processes.map(async (process) => ({
    ...process,
    identity: await processIdentity(process.pid),
  })))
  return identified.filter((process) => process.identity !== null)
}

async function processIdentity(pid) {
  try {
    if (process.platform === 'win32') {
      const script = `Get-CimInstance Win32_Process -Filter "ProcessId = ${pid}" | Select-Object CreationDate,CommandLine | ConvertTo-Json -Compress`
      const { stdout } = await execFileAsync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
        timeout: 5000,
        maxBuffer: 1024 * 1024,
      })
      if (stdout.trim() === '') return null
      const processInfo = JSON.parse(stdout)
      return `${processInfo.CreationDate || ''}|${processInfo.CommandLine || ''}`
    }
    const { stdout } = await execFileAsync('ps', ['-p', String(pid), '-o', 'lstart=,command='], {
      timeout: 5000,
      maxBuffer: 1024 * 1024,
    })
    return stdout.trim() || null
  } catch {
    return null
  }
}

async function stopDaemonProcesses(daemons) {
  const runningProcesses = new Map(daemons.map((daemon) => [daemon.pid, daemon.identity]))
  const refresh = async () => {
    for (const [pid, identity] of runningProcesses) {
      if (await processIdentity(pid) !== identity) runningProcesses.delete(pid)
    }
  }
  const signal = (pid, name) => {
    try {
      process.kill(pid, name)
      return true
    } catch (error) {
      if (error.code === 'ESRCH') {
        runningProcesses.delete(pid)
        return true
      }
      console.error(`Unable to send ${name} to incompatible Porto daemon ${pid}`, error)
      return false
    }
  }
  const waitForExit = async (attempts) => {
    for (let attempt = 0; attempt < attempts; attempt += 1) {
      await delay(250)
      await refresh()
      if (runningProcesses.size === 0 && !(await inspectDaemon({ daemonURL: DAEMON_URL })).reachable) {
        return true
      }
    }
    return false
  }
  await refresh()
  for (const pid of runningProcesses.keys()) {
    if (!signal(pid, 'SIGTERM')) return false
  }
  if (await waitForExit(40)) return true
  await refresh()
  for (const pid of runningProcesses.keys()) {
    if (!signal(pid, 'SIGKILL')) return false
  }
  return waitForExit(20)
}

async function ensureDaemonRunning() {
  let existing = await inspectDaemon({ daemonURL: DAEMON_URL })
  if (existing.ready && !app.isPackaged) return true
  let processes
  try {
    processes = await runningDaemonProcesses()
  } catch (error) {
    console.error('Unable to inspect existing Porto daemons', error)
    return false
  }
  if (existing.ready) {
    const bundledExecutable = normalizedExecutablePath(portoBinary())
    if (processes.length === 0 || processes.some((process) => normalizedExecutablePath(process.executable) === bundledExecutable)) {
      return true
    }
  }
  if (processes.length > 0) {
    if (!existing.reachable) {
      for (let attempt = 0; attempt < 10; attempt += 1) {
        await delay(300)
        existing = await inspectDaemon({ daemonURL: DAEMON_URL })
        if (existing.ready) {
          const bundledExecutable = normalizedExecutablePath(portoBinary())
          if (processes.some((process) => normalizedExecutablePath(process.executable) === bundledExecutable)) {
            return true
          }
        }
        if (existing.reachable) break
      }
    }
    if (!(await stopDaemonProcesses(processes))) return false
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
  if (!status.enabled) return
  const command = dockerBootstrapCommand(status, {
    isPackaged: app.isPackaged,
    platform: process.platform,
  })
  if (command !== null) {
    await installDockerEngine({ daemonURL: DAEMON_URL })
    for (let attempt = 0; attempt < 30; attempt += 1) {
      status = await inspectDockerStatus({ daemonURL: DAEMON_URL })
      if (status.available) break
      await delay(500)
    }
    if (!status.available) {
      throw new Error(status.message || 'Porto container runtime did not become available')
    }
  }
  if (app.isPackaged && process.platform !== 'win32' && status.available) {
    await installDockerContext({ daemonURL: DAEMON_URL })
  }
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
  let blankReloadAttempts = 0
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
  window.webContents.on('did-finish-load', () => {
    setTimeout(async () => {
      if (window.isDestroyed() || !window.webContents.getURL().startsWith(DAEMON_URL)) return
      try {
        const rootChildren = await window.webContents.executeJavaScript(
          'document.getElementById("root")?.childElementCount ?? 0',
          true,
        )
        const action = dashboardLoadAction({ rootChildren, reloadAttempts: blankReloadAttempts })
        if (action === 'ready') {
          blankReloadAttempts = 0
          return
        }
        if (action === 'reload') {
          blankReloadAttempts += 1
          window.webContents.reloadIgnoringCache()
          return
        }
        console.error('Porto dashboard loaded without rendering any content')
        const html = `<!doctype html><html><body style="margin:0;background:#252925;color:#dedfd7;font:14px system-ui,sans-serif"><main style="padding:38px"><h1 style="color:#fffdf7">Dashboard failed to render</h1><p>Restart Porto. If this continues, reinstall the latest build.</p></main></body></html>`
        window.loadURL(`data:text/html;charset=utf-8,${encodeURIComponent(html)}`)
      } catch (error) {
        console.error('Unable to verify Porto dashboard rendering', error)
      }
    }, 500)
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
  if (!bundledPortoBinaryReady()) {
    dialog.showErrorBox(
      'Porto installation incomplete',
      `The bundled Porto daemon is missing or is not executable at ${portoBinary()}. Reinstall Porto from the DMG.`,
    )
    app.quit()
    return
  }
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
