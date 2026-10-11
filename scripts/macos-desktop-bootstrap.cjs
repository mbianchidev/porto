const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const path = require('node:path')

const LIBRARY = 'libporto-power-notification.dylib'
const LIBRARY_REFERENCE = `@rpath/${LIBRARY}`
const BOOTSTRAP_IMPORTS = new Set([
  '_ElectronInitializeICUandStartNode',
  '_ElectronMain',
  '__ZN8electron5fuses18IsRunAsNodeEnabledEv',
  '___error',
  '___stderrp',
  '___stdinp',
  '___stdoutp',
  '_freopen',
  '_fstat',
  '_fstat$INODE64',
  '_getenv',
  'dyld_stub_binder',
])

function run(command, args) {
  const result = spawnSync(command, args, { encoding: 'utf8', timeout: 60000 })
  if (result.error) throw new Error(`Unable to run ${command}: ${result.error.message}`)
  if (result.status !== 0) {
    throw new Error(`${command} ${args.join(' ')} failed (${result.signal || result.status}): ${result.stderr}`)
  }
  return result.stdout.trim()
}

function architectures(executable) {
  return run('xcrun', ['lipo', '-archs', executable]).split(/\s+/).sort()
}

function validateMacOSBootstrap(bundle, { executableName = 'Porto' } = {}) {
  if (!['Porto', 'Electron'].includes(executableName)) throw new Error('Invalid Porto macOS bootstrap executable')
  const executable = path.join(bundle, 'Contents', 'MacOS', executableName)
  const library = path.join(bundle, 'Contents', 'Frameworks', LIBRARY)
  if (!fs.statSync(library).isFile()) throw new Error(`Missing packaged macOS power guard: ${library}`)
  if (!run('xcrun', ['otool', '-L', executable]).includes(LIBRARY_REFERENCE)) {
    throw new Error(`Packaged Porto executable does not load ${LIBRARY_REFERENCE}`)
  }
  if (!run('xcrun', ['otool', '-l', library]).includes('sectname __interpose')) {
    throw new Error(`Packaged macOS power guard has no interposition section: ${library}`)
  }
  if (architectures(executable).join(' ') !== architectures(library).join(' ')) {
    throw new Error('Packaged Porto executable and power guard architectures differ')
  }
}

function installMacOSBootstrap(bundle, { executableName = 'Porto' } = {}) {
  if (process.platform !== 'darwin') {
    throw new Error('Package macOS Porto on a macOS host so the native startup guard is included')
  }
  if (!['Porto', 'Electron'].includes(executableName)) throw new Error('Invalid Porto macOS bootstrap executable')
  const executable = path.join(bundle, 'Contents', 'MacOS', executableName)
  const frameworks = path.join(bundle, 'Contents', 'Frameworks')
  if (fs.existsSync(path.join(frameworks, LIBRARY))) {
    validateMacOSBootstrap(bundle, { executableName })
    return
  }
  const targets = architectures(executable)
  if (targets.some((arch) => !['arm64', 'x86_64'].includes(arch))) {
    throw new Error(`Unsupported Porto macOS architectures: ${targets.join(' ')}`)
  }
  for (const arch of targets) {
    const imports = run('xcrun', ['nm', '-u', '-arch', arch, executable]).split(/\r?\n/)
    const unexpected = imports.map((symbol) => symbol.trim())
      .filter((symbol) => symbol && !BOOTSTRAP_IMPORTS.has(symbol))
    if (unexpected.length > 0) {
      throw new Error(`Electron's macOS bootstrap changed; review its startup contract before packaging: ${unexpected.join(', ')}`)
    }
  }
  const minimumVersion = run('/usr/libexec/PlistBuddy', [
    '-c', 'Print :LSMinimumSystemVersion', path.join(bundle, 'Contents', 'Info.plist'),
  ])
  if (!/^\d+\.\d+(?:\.\d+)?$/.test(minimumVersion)) {
    throw new Error(`Invalid minimum macOS version: ${minimumVersion}`)
  }
  const temporary = fs.mkdtempSync(path.join(path.dirname(executable), '.porto-bootstrap-'))
  try {
    const library = path.join(temporary, LIBRARY)
    const main = path.join(temporary, executableName)
    const flags = [
      '-Wall', '-Wextra', '-Werror', '-O2',
      ...targets.flatMap((arch) => ['-arch', arch]),
      `-mmacosx-version-min=${minimumVersion}`,
    ]
    run('xcrun', [
      'clang', ...flags, '-dynamiclib',
      path.join(__dirname, '..', 'hacks', 'macos-power-notification-compat.c'),
      '-framework', 'IOKit', '-framework', 'CoreFoundation',
      '-install_name', LIBRARY_REFERENCE, '-o', library,
    ])
    run('xcrun', [
      'clang++', ...flags, '-std=c++17', path.join(__dirname, 'macos-desktop-main.cc'),
      '-F', frameworks, '-framework', 'Electron Framework',
      '-Xlinker', '-needed_library', '-Xlinker', library,
      '-Wl,-rpath,@executable_path/../Frameworks', '-o', main,
    ])
    if (architectures(main).join(' ') !== targets.join(' ')) {
      throw new Error('The native Porto bootstrap changed the packaged architectures')
    }
    // Complete all native edits before any future application signing/notarization.
    fs.renameSync(library, path.join(frameworks, LIBRARY))
    fs.renameSync(main, executable)
    validateMacOSBootstrap(bundle, { executableName })
  } finally {
    fs.rmSync(temporary, { recursive: true, force: true })
  }
}

if (require.main === module) {
  const [mode, bundle] = process.argv.slice(2)
  if (mode !== '--verify' || !bundle || process.argv.length !== 4) {
    console.error('usage: macos-desktop-bootstrap.cjs --verify <Porto.app>')
    process.exitCode = 2
  } else {
    try {
      validateMacOSBootstrap(bundle)
    } catch (error) {
      console.error(error.message)
      process.exitCode = 1
    }
  }
}

module.exports = { installMacOSBootstrap, validateMacOSBootstrap }
