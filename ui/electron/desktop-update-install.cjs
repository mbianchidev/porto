const { spawn } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')

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
      ],
      helperPath,
      packagePath,
      destination,
      executable,
      errorFile,
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
  })
  const child = spawnImpl(invocation.command, invocation.args, {
    detached: true,
    stdio: 'ignore',
    windowsHide: true,
  })
  await new Promise((resolve, reject) => {
    child.once('error', reject)
    child.once('spawn', resolve)
  })
  child.unref()
  return installation
}

module.exports = {
  currentInstallation,
  installerCommand,
  launchDownloadedUpdate,
}
