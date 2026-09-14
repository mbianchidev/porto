// Secure Porto desktop shell.
//
// This process never talks to the daemon's data on its own: it only opens a
// window pointed at the local daemon's web UI, and starts the bundled daemon when
// the endpoint is unreachable or belongs to a different binary build.
// contextIsolation stays on and nodeIntegration stays off. The preload exposes
// only narrow desktop-preferences and update bridges used by the Settings page.
const { app, BrowserWindow, dialog, ipcMain, Menu, nativeImage, shell, Tray } = require('electron')
const { execFile, spawn } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')
const { promisify } = require('node:util')

const {
  bundledExecutablePaths,
  daemonBinaryIdentity,
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
  waitForDockerEngine,
} = require('./daemon-readiness.cjs')
const {
  DEFAULT_DESKTOP_PREFERENCES,
  loadDesktopPreferences,
  loginItemOptions,
  loginItemSupported,
  saveDesktopPreferences,
  shouldHideWindow,
  shouldStartHidden,
  validateDesktopPreferences,
} = require('./desktop-preferences.cjs')
const { launchDownloadedUpdate } = require('./desktop-update-install.cjs')
const {
  DEFAULT_UPDATE_CHECK_INTERVAL,
  createDesktopUpdater,
  readPackagedReleaseVersion,
} = require('./desktop-updater.cjs')

const APP_NAME = 'Porto'
const APP_ID = 'dev.mbianchi.porto'
const APP_ICON = path.join(__dirname, 'assets', 'porto.png')
const DAEMON_URL = 'http://127.0.0.1:37623'
const windows = new Set()
const execFileAsync = promisify(execFile)
let desktopPreferences = { ...DEFAULT_DESKTOP_PREFERENCES }
let desktopPreferencesPath = ''
let desktopUpdater = null
let updateCheckTimer = null
let updateInstallErrorPath = ''
let updatePromptActive = false
let promptedAvailableVersion = ''
let promptedDownloadedVersion = ''
let tray = null
let quitting = false

app.setName(APP_NAME)
process.title = APP_NAME

function delay(milliseconds) {
  return new Promise((resolve) => setTimeout(resolve, milliseconds))
}

function loginSupported() {
  if (!loginItemSupported(process.platform, app.isPackaged)) return false
  return process.platform !== 'darwin' || app.isInApplicationsFolder()
}

function getLoginItemSettings(preferences) {
  if (!loginSupported()) return {}
  if (process.platform === 'win32') {
    const options = loginItemOptions(preferences, process.platform, app.getPath('exe'))
    return app.getLoginItemSettings({
      path: options.path,
      args: options.args,
    })
  }
  return app.getLoginItemSettings()
}

function desktopPreferencesSnapshot() {
  const supported = loginSupported()
  return {
    ...desktopPreferences,
    openAtLogin: supported ? getLoginItemSettings(desktopPreferences).openAtLogin === true : false,
    loginItemSupported: supported,
  }
}

function applyLoginItemSettings(preferences) {
  if (!loginSupported()) {
    if (preferences.openAtLogin) {
      throw new Error('Open at login is available only in an installed macOS or Windows desktop build.')
    }
    return
  }
  const options = loginItemOptions(preferences, process.platform, app.getPath('exe'))
  app.setLoginItemSettings(options)
  const actual = getLoginItemSettings(preferences).openAtLogin === true
  if (actual !== preferences.openAtLogin) {
    throw new Error(`Unable to ${preferences.openAtLogin ? 'enable' : 'disable'} opening Porto at login.`)
  }
}

function presentWindow(window) {
  if (process.platform === 'darwin') app.dock?.show()
  app.focus({ steal: true })
  if (window.isMinimized()) window.restore()
  window.show()
  window.focus()
  window.moveTop()
  void presentPendingUpdatePrompt(window)
}

function mainWindow() {
  for (const window of windows) {
    if (!window.isDestroyed()) return window
  }
  return null
}

function showMainWindow() {
  const window = mainWindow()
  if (window) {
    presentWindow(window)
    return
  }
  createWindow()
}

function configureTray(enabled) {
  if (!enabled) {
    tray?.destroy()
    tray = null
    return
  }
  if (tray !== null) return
  const size = process.platform === 'darwin' ? 18 : 20
  const icon = nativeImage.createFromPath(APP_ICON).resize({ width: size, height: size })
  if (icon.isEmpty()) {
    throw new Error(`Unable to load Porto tray icon from ${APP_ICON}`)
  }
  tray = new Tray(icon)
  tray.setToolTip(APP_NAME)
  tray.setContextMenu(Menu.buildFromTemplate([
    {
      label: 'Open Porto',
      click: showMainWindow,
    },
    { type: 'separator' },
    {
      label: 'Quit Porto',
      click: () => {
        quitting = true
        app.quit()
      },
    },
  ]))
  tray.on('click', showMainWindow)
}

function assertTrustedDashboardRequest(event) {
  const senderURL = event.senderFrame?.url
  try {
    if (new URL(senderURL).origin === DAEMON_URL) return
  } catch {
    // Fall through to the explicit rejection below.
  }
  throw new Error('Desktop integrations are available only to the Porto dashboard.')
}

function broadcastUpdateStatus(status) {
  for (const window of windows) {
    if (!window.isDestroyed()) window.webContents.send('porto:updates:status', status)
  }
}

async function downloadAvailableUpdate() {
  if (desktopUpdater === null) return
  try {
    await desktopUpdater.download()
  } catch (error) {
    console.error('Unable to download the Porto update', error)
  }
}

function handleDesktopUpdateStatus(status) {
  broadcastUpdateStatus(status)
  if (status.phase === 'available' && desktopPreferences.automaticallyDownloadUpdates) {
    void downloadAvailableUpdate()
    return
  }
  if (status.phase === 'available' || status.phase === 'downloaded') {
    void presentPendingUpdatePrompt()
  }
}

async function restartAndInstallUpdate() {
  if (desktopUpdater === null) throw new Error('The Porto updater is not ready')
  const update = await desktopUpdater.downloadedUpdate()
  await launchDownloadedUpdate({
    platform: process.platform,
    executablePath: app.getPath('exe'),
    packagePath: update.packagePath,
    downloadsDirectory: path.dirname(update.packagePath),
    errorFile: updateInstallErrorPath,
    helperDirectory: process.resourcesPath,
  })
  desktopUpdater.markInstalling()
  quitting = true
  app.quit()
  return desktopUpdater.getStatus()
}

async function presentPendingUpdatePrompt(window = mainWindow()) {
  if (
    desktopUpdater === null
    || updatePromptActive
    || !window
    || window.isDestroyed()
    || !window.isVisible()
  ) return

  const status = desktopUpdater.getStatus()
  if (status.phase === 'downloaded' && status.availableVersion !== promptedDownloadedVersion) {
    promptedDownloadedVersion = status.availableVersion
    updatePromptActive = true
    let response = 1
    try {
      ({ response } = await dialog.showMessageBox(window, {
        type: 'info',
        title: 'Porto update ready',
        message: `Porto ${status.availableVersion} is ready to install.`,
        detail: 'Restart Porto to update the desktop app and bundled daemon. Running VMs, Kubernetes clusters, and containers remain in place while Porto reconnects.',
        buttons: ['Restart and update', 'Later'],
        defaultId: 0,
        cancelId: 1,
        noLink: true,
      }))
    } finally {
      updatePromptActive = false
    }
    if (response === 0) {
      try {
        await restartAndInstallUpdate()
      } catch (error) {
        console.error('Unable to restart Porto for the downloaded update', error)
        dialog.showErrorBox('Porto update failed', error.message)
      }
    }
    return
  }

  if (
    status.phase === 'available'
    && !desktopPreferences.automaticallyDownloadUpdates
    && status.availableVersion !== promptedAvailableVersion
  ) {
    promptedAvailableVersion = status.availableVersion
    updatePromptActive = true
    let response = 1
    try {
      ({ response } = await dialog.showMessageBox(window, {
        type: 'info',
        title: 'Porto update available',
        message: `Porto ${status.availableVersion} is available.`,
        detail: 'Download the verified update in Porto now, or leave it for later from Settings.',
        buttons: ['Download update', 'Later'],
        defaultId: 0,
        cancelId: 1,
        noLink: true,
      }))
    } finally {
      updatePromptActive = false
    }
    if (response === 0) void downloadAvailableUpdate()
  }
}

function registerDesktopIPC() {
  ipcMain.handle('porto:desktop-preferences:get', (event) => {
    assertTrustedDashboardRequest(event)
    return desktopPreferencesSnapshot()
  })
  ipcMain.handle('porto:desktop-preferences:set', (event, value) => {
    assertTrustedDashboardRequest(event)
    const next = validateDesktopPreferences(value)
    const previous = { ...desktopPreferences }
    try {
      applyLoginItemSettings(next)
      configureTray(next.keepInTray)
      saveDesktopPreferences(desktopPreferencesPath, next)
      desktopPreferences = next
      if (next.automaticallyDownloadUpdates && desktopUpdater?.getStatus().phase === 'available') {
        void downloadAvailableUpdate()
      }
      return desktopPreferencesSnapshot()
    } catch (error) {
      try {
        applyLoginItemSettings(previous)
        configureTray(previous.keepInTray)
      } catch (rollbackError) {
        console.error('Unable to roll back desktop preference changes', rollbackError)
      }
      throw error
    }
  })
  ipcMain.handle('porto:updates:get-status', (event) => {
    assertTrustedDashboardRequest(event)
    if (desktopUpdater === null) throw new Error('The Porto updater is not ready')
    return desktopUpdater.getStatus()
  })
  ipcMain.handle('porto:updates:check', async (event) => {
    assertTrustedDashboardRequest(event)
    if (desktopUpdater === null) throw new Error('The Porto updater is not ready')
    return desktopUpdater.check()
  })
  ipcMain.handle('porto:updates:download', async (event) => {
    assertTrustedDashboardRequest(event)
    if (desktopUpdater === null) throw new Error('The Porto updater is not ready')
    return desktopUpdater.download()
  })
  ipcMain.handle('porto:updates:restart-and-install', async (event) => {
    assertTrustedDashboardRequest(event)
    return restartAndInstallUpdate()
  })
}

function startUpdateChecks() {
  if (desktopUpdater === null) return
  const check = () => {
    void desktopUpdater.check().catch((error) => {
      console.error('Unable to check GitHub Releases for Porto updates', error)
    })
  }
  const initialCheck = setTimeout(check, 3000)
  initialCheck.unref?.()
  updateCheckTimer = setInterval(check, DEFAULT_UPDATE_CHECK_INTERVAL)
  updateCheckTimer.unref?.()
}

function consumeUpdateInstallError() {
  try {
    const message = fs.readFileSync(updateInstallErrorPath, 'utf8').trim()
    fs.rmSync(updateInstallErrorPath, { force: true })
    return message
  } catch (error) {
    if (error.code !== 'ENOENT') console.error('Unable to read the previous Porto update error', error)
    return ''
  }
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
  const bundledPaths = bundledExecutablePaths(process.resourcesPath)
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
      windowsHide: true,
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
      windowsHide: true,
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
        windowsHide: true,
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
  let expectedDaemonIdentity = ''
  if (app.isPackaged) {
    try {
      expectedDaemonIdentity = daemonBinaryIdentity(portoBinary())
    } catch (error) {
      console.error('Unable to identify the bundled Porto daemon', error)
      return false
    }
  }
  const inspectExpectedDaemon = () => inspectDaemon({
    daemonURL: DAEMON_URL,
    expectedDaemonIdentity,
  })
  let existing = await inspectExpectedDaemon()
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
        existing = await inspectExpectedDaemon()
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
      if ((await inspectExpectedDaemon()).ready) return true
    }
  }
  return false
}

async function ensureDockerEngine() {
  let status = await inspectDockerStatus({ daemonURL: DAEMON_URL })
  if (!status.enabled) return
  const command = dockerBootstrapCommand(status, {
    isPackaged: app.isPackaged,
  })
  if (command !== null) {
    await installDockerEngine({ daemonURL: DAEMON_URL })
    status = await waitForDockerEngine({ daemonURL: DAEMON_URL })
  }
  if (app.isPackaged && process.platform !== 'win32' && status.available) {
    await installDockerContext({ daemonURL: DAEMON_URL })
  }
}

function createBootstrapWindow(show = true) {
  const window = new BrowserWindow({
    width: 460,
    height: 210,
    resizable: false,
    show,
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
  window.on('close', (event) => {
    if (!shouldHideWindow(desktopPreferences, quitting)) return
    event.preventDefault()
    window.hide()
    if (process.platform === 'darwin') app.dock?.hide()
  })
  window.on('query-session-end', () => {
    quitting = true
  })
  window.on('closed', () => windows.delete(window))
  presentWindow(window)
  window.once('ready-to-show', () => presentWindow(window))
  window.loadURL(DAEMON_URL).then(() => {
    presentWindow(window)
  }).catch((error) => {
    console.error('Unable to load Porto dashboard', error)
  })
}

const hasLock = app.requestSingleInstanceLock()
if (!hasLock) {
  app.quit()
}

app.on('second-instance', () => {
  showMainWindow()
})

app.whenReady().then(async () => {
  app.setAppUserModelId(APP_ID)
  if (process.platform === 'darwin') app.dock?.setIcon(APP_ICON)
  desktopPreferencesPath = path.join(app.getPath('userData'), 'desktop-preferences.json')
  const updatesDirectory = path.join(app.getPath('userData'), 'updates')
  updateInstallErrorPath = path.join(updatesDirectory, 'install-error.txt')
  desktopPreferences = loadDesktopPreferences(desktopPreferencesPath, (error) => {
    console.error('Unable to load desktop preferences; using defaults', error)
  })
  let loginSettings = {}
  if (loginSupported()) {
    try {
      loginSettings = getLoginItemSettings(desktopPreferences)
      desktopPreferences.openAtLogin = loginSettings.openAtLogin === true
    } catch (error) {
      desktopPreferences.openAtLogin = false
      console.error('Unable to inspect the Porto login item', error)
    }
  } else {
    desktopPreferences.openAtLogin = false
  }
  let currentReleaseVersion = app.getVersion()
  try {
    currentReleaseVersion = readPackagedReleaseVersion({
      isPackaged: app.isPackaged,
      appVersion: app.getVersion(),
      resourcesPath: process.resourcesPath,
    })
  } catch (error) {
    console.error('Unable to read the packaged Porto release version; using application metadata', error)
  }
  desktopUpdater = createDesktopUpdater({
    currentVersion: currentReleaseVersion,
    platform: process.platform,
    arch: process.arch,
    isPackaged: app.isPackaged,
    downloadsDirectory: updatesDirectory,
    onStatus: handleDesktopUpdateStatus,
  })
  const updateInstallError = consumeUpdateInstallError()
  registerDesktopIPC()
  const startHidden = shouldStartHidden(desktopPreferences, process.argv)
  if (startHidden && process.platform === 'darwin') app.dock?.hide()
  if (!bundledPortoBinaryReady()) {
    dialog.showErrorBox(
      'Porto installation incomplete',
      `The bundled Porto daemon is missing or is not executable at ${portoBinary()}. Reinstall Porto from the DMG.`,
    )
    app.quit()
    return
  }
  const bootstrapWindow = createBootstrapWindow(!startHidden)
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
  let trayError = null
  try {
    configureTray(desktopPreferences.keepInTray)
  } catch (error) {
    trayError = error
    desktopPreferences.keepInTray = false
    console.error('Unable to initialize the Porto tray icon', error)
    try {
      saveDesktopPreferences(desktopPreferencesPath, desktopPreferences)
    } catch (saveError) {
      console.error('Unable to persist the tray fallback', saveError)
    }
  }
  if (!startHidden || trayError !== null) createWindow()
  bootstrapWindow.close()
  if (trayError !== null) {
    dialog.showErrorBox('Porto tray unavailable', trayError.message)
  }
  if (dockerError !== null) {
    dialog.showErrorBox('Porto container runtime unavailable', dockerError.message)
  }
  if (updateInstallError !== '') {
    dialog.showErrorBox('Porto update failed', updateInstallError)
  }
  startUpdateChecks()

  app.on('activate', () => {
    showMainWindow()
  })
})

app.on('before-quit', () => {
  quitting = true
  if (updateCheckTimer !== null) clearInterval(updateCheckTimer)
})

app.on('window-all-closed', () => {
  // Intentionally does not stop the Porto daemon: it keeps managing projects,
  // containers, clusters, and VMs regardless of whether the window is open.
  if (!desktopPreferences.keepInTray && process.platform !== 'darwin') app.quit()
})
