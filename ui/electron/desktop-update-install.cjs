const { spawn } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')
const { setTimeout: delay } = require('node:timers/promises')

const UPDATE_HELPER_READY_TIMEOUT = 15 * 1000

function currentInstallation(platform, executablePath) {
  if (typeof executablePath !== 'string' || executablePath === '') {
    throw new Error('Porto executable path is required')
  }
  if (platform === 'darwin') {
    const macPath = path.posix
    const macOSDirectory = macPath.dirname(executablePath)
    const contentsDirectory = macPath.dirname(macOSDirectory)
    const bundle = macPath.dirname(contentsDirectory)
    if (
      macPath.basename(macOSDirectory) !== 'MacOS'
      || macPath.basename(contentsDirectory) !== 'Contents'
      || macPath.extname(bundle) !== '.app'
    ) {
      throw new Error(`Porto is not running from a replaceable macOS application bundle: ${executablePath}`)
    }
    return { destination: bundle, executable: executablePath }
  }
  if (platform === 'linux') {
    const destination = path.posix.dirname(executablePath)
    if (destination === '/' || path.posix.basename(executablePath) !== 'Porto') {
      throw new Error(`Porto is not running from a replaceable Linux installation: ${executablePath}`)
    }
    return { destination, executable: executablePath }
  }
  if (platform === 'win32') {
    const destination = path.win32.dirname(executablePath)
    if (
      path.win32.parse(destination).root === destination
      || path.win32.basename(executablePath).toLowerCase() !== 'porto.exe'
    ) {
      throw new Error(`Porto is not running from a replaceable Windows installation: ${executablePath}`)
    }
    return { destination, executable: executablePath }
  }
  throw new Error(`Porto update installation is not supported on ${platform}`)
}

function installerCommand({
  platform,
  helperPath,
  parentPid,
  packagePath,
  destination,
  executable,
  errorFile,
  archiveRootName,
  readyFile,
  proceedFile,
}) {
  if (platform === 'win32') {
    return {
      command: 'powershell.exe',
      args: [
        '-NoProfile',
        '-NonInteractive',
        '-ExecutionPolicy',
        'Bypass',
        '-File',
        helperPath,
        '-ParentPid',
        String(parentPid),
        '-PackagePath',
        packagePath,
        '-InstallDirectory',
        destination,
        '-ExecutablePath',
        executable,
        '-ErrorFile',
        errorFile,
        '-ReadyFile',
        readyFile,
        '-ProceedFile',
        proceedFile,
      ],
      helperPath,
      packagePath,
      destination,
      executable,
      errorFile,
      readyFile,
      proceedFile,
    }
  }
  return {
    command: '/bin/bash',
    args: [
      helperPath,
      platform,
      String(parentPid),
      packagePath,
      destination,
      executable,
      errorFile,
      archiveRootName,
    ],
    helperPath,
    packagePath,
    destination,
    executable,
    errorFile,
  }
}

async function waitForUpdateHelperReady(child, readyFile, {
  timeoutMs = UPDATE_HELPER_READY_TIMEOUT,
  errorFile = '',
  existsImpl = fs.existsSync,
  readFileImpl = fs.readFileSync,
  delayImpl = delay,
  nowImpl = Date.now,
} = {}) {
  const helperError = () => {
    if (errorFile === '') return ''
    try {
      return readFileImpl(errorFile, 'utf8').trim()
    } catch (error) {
      if (error.code === 'ENOENT') return ''
      throw error
    }
  }
  const deadline = nowImpl() + timeoutMs
  while (nowImpl() < deadline) {
    if (existsImpl(readyFile)) return
    const message = helperError()
    if (message !== '') throw new Error(message)
    if (child.exitCode != null || child.signalCode != null) {
      const outcome = child.exitCode != null ? `code ${child.exitCode}` : `signal ${child.signalCode}`
      throw new Error(`Porto update helper exited before becoming ready with ${outcome}`)
    }
    await delayImpl(50)
  }
  const message = helperError()
  if (message !== '') throw new Error(message)
  throw new Error(`Porto update helper did not become ready within ${timeoutMs / 1000}s`)
}

function expectedExtension(platform) {
  if (platform === 'darwin') return '.dmg'
  if (platform === 'win32') return '.exe'
  if (platform === 'linux') return '.tar.gz'
  throw new Error(`Porto update installation is not supported on ${platform}`)
}

function helperName(platform) {
  return platform === 'win32' ? 'apply-update.ps1' : 'apply-update.sh'
}

async function launchDownloadedUpdate({
  platform,
  executablePath,
  packagePath,
  downloadsDirectory,
  errorFile,
  helperDirectory = __dirname,
  parentPid = process.pid,
  spawnImpl = spawn,
} = {}) {
  const installation = currentInstallation(platform, executablePath)
  const extension = expectedExtension(platform)
  if (typeof packagePath !== 'string' || !packagePath.endsWith(extension)) {
    throw new Error(`Porto ${platform} updates must use a ${extension} package`)
  }
  fs.accessSync(packagePath, fs.constants.R_OK)
  const destinationParent = platform === 'win32'
    ? path.win32.dirname(installation.destination)
    : path.posix.dirname(installation.destination)
  fs.accessSync(destinationParent, fs.constants.W_OK)
  if (platform === 'win32' && fs.existsSync(installation.destination)) {
    fs.accessSync(installation.destination, fs.constants.W_OK)
  }
  fs.mkdirSync(downloadsDirectory, { recursive: true, mode: 0o700 })
  fs.rmSync(errorFile, { force: true })

  const sourceHelper = path.join(helperDirectory, helperName(platform))
  const helperPath = path.join(
    downloadsDirectory,
    platform === 'win32' ? `apply-update-${parentPid}.ps1` : `apply-update-${parentPid}.sh`,
  )
  fs.copyFileSync(sourceHelper, helperPath)
  if (platform !== 'win32') fs.chmodSync(helperPath, 0o700)
  const readyFile = platform === 'win32' ? `${helperPath}.ready` : ''
  const proceedFile = platform === 'win32' ? `${helperPath}.proceed` : ''
  if (platform === 'win32') {
    fs.rmSync(readyFile, { force: true })
    fs.rmSync(proceedFile, { force: true })
  }

  const packageName = platform === 'win32' ? path.win32.basename(packagePath) : path.posix.basename(packagePath)
  const archiveRootName = platform === 'linux' ? packageName.slice(0, -extension.length) : ''
  if (platform === 'linux' && !/^porto-desktop_[0-9A-Za-z.+-]+_linux_(?:amd64|arm64)$/.test(archiveRootName)) {
    throw new Error(`Unexpected Porto Linux update archive: ${packageName}`)
  }
  const invocation = installerCommand({
    platform,
    helperPath,
    parentPid,
    packagePath,
    destination: installation.destination,
    executable: installation.executable,
    errorFile,
    archiveRootName,
    readyFile,
    proceedFile,
  })
  const child = spawnImpl(invocation.command, invocation.args, {
    cwd: downloadsDirectory,
    detached: true,
    stdio: 'ignore',
    windowsHide: true,
  })
  await new Promise((resolve, reject) => {
    child.once('error', reject)
    child.once('spawn', resolve)
  })
  if (platform === 'win32') {
    try {
      await waitForUpdateHelperReady(child, readyFile, { errorFile })
    } catch (error) {
      if (child.exitCode == null && child.signalCode == null) {
        try {
          child.kill()
        } catch (killError) {
          throw new Error(
            `${error.message}; unable to stop failed update helper: ${killError.message}`,
            { cause: error },
          )
        }
      }
      throw error
    }
  }
  child.unref()
  return {
    ...installation,
    helperPid: child.pid,
    proceed() {
      if (platform === 'win32') {
        fs.writeFileSync(proceedFile, 'proceed\n', { encoding: 'utf8', mode: 0o600 })
      }
    },
  }
}

module.exports = {
  UPDATE_HELPER_READY_TIMEOUT,
  currentInstallation,
  installerCommand,
  launchDownloadedUpdate,
  waitForUpdateHelperReady,
}
