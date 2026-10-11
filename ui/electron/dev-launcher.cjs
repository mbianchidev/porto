const path = require('node:path')
const os = require('node:os')
const { spawn } = require('node:child_process')

const { prepareMacOSDevelopmentApp } = require('../../scripts/macos-development-app.cjs')

function developmentLaunch({
  platform = process.platform,
  appDirectory,
  electronExecutable,
  developmentBundle,
  args = [],
  environment = process.env,
}) {
  const directory = path.resolve(appDirectory)
  const launchEnvironment = { ...environment }
  delete launchEnvironment.ELECTRON_RUN_AS_NODE
  delete launchEnvironment.ELECTRON_FORCE_IS_PACKAGED
  if (platform === 'darwin') {
    if (!developmentBundle) throw new Error('The guarded macOS development application is required')
    return {
      executable: path.join(developmentBundle, 'Contents', 'MacOS', 'Electron'),
      args: [...args], cwd: directory, environment: launchEnvironment,
    }
  }
  return {
    executable: electronExecutable, args: [directory, ...args],
    cwd: directory, environment: launchEnvironment,
  }
}

async function launchDevelopmentApp({
  appDirectory = __dirname,
  electronExecutable = require('electron'),
  args = process.argv.slice(2),
  environment = process.env,
  platform = process.platform,
  cacheRoot,
  spawnImpl = spawn,
} = {}) {
  const bundle = platform === 'darwin'
    ? await prepareMacOSDevelopmentApp({ appDirectory, electronExecutable, cacheRoot })
    : undefined
  const launch = developmentLaunch({
    platform, appDirectory, electronExecutable, developmentBundle: bundle, args, environment,
  })
  if (bundle) console.error(`Porto Dev: ${bundle}\nReopen this application, not node_modules/Electron.app.`)
  return new Promise((resolve, reject) => {
    const child = spawnImpl(launch.executable, launch.args, {
      cwd: launch.cwd, env: launch.environment, stdio: 'inherit',
    })
    const forward = (signal) => child.kill(signal)
    const terminate = () => forward('SIGTERM')
    const interrupt = () => forward('SIGINT')
    process.on('SIGTERM', terminate)
    process.on('SIGINT', interrupt)
    const cleanup = () => {
      process.removeListener('SIGTERM', terminate)
      process.removeListener('SIGINT', interrupt)
    }
    child.once('error', (error) => {
      cleanup()
      reject(new Error(`Unable to start Porto development desktop: ${error.message}`, { cause: error }))
    })
    child.once('exit', (code, signal) => {
      cleanup()
      if (signal) console.error(`Porto development desktop exited unexpectedly (${signal}).`)
      resolve(code ?? (signal ? 128 + (os.constants.signals[signal] || 1) : 1))
    })
  })
}

if (require.main === module) {
  launchDevelopmentApp().then((code) => { process.exitCode = code }).catch((error) => {
    console.error(error.message)
    process.exitCode = 1
  })
}

module.exports = { developmentLaunch, launchDevelopmentApp, prepareMacOSDevelopmentApp }
