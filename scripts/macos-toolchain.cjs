const { spawnSync } = require('node:child_process')

const defaultDeveloperDirectory = '/Applications/Xcode.app/Contents/Developer'

function commandFailure(result) {
  return result.error?.message || result.stderr?.trim()
    || `command exited with ${result.signal ?? result.status}`
}

function macOSPackagingEnvironment(environment = process.env, run = spawnSync) {
  const failures = []
  const execute = (command, args, env) => run(command, args, {
    encoding: 'utf8', env, timeout: 30000,
  })
  const candidates = []
  if (environment.DEVELOPER_DIR) {
    candidates.push(environment.DEVELOPER_DIR)
  } else {
    const selected = execute('xcode-select', ['--print-path'], environment)
    if (!selected.error && selected.status === 0 && selected.stdout?.trim()) {
      candidates.push(selected.stdout.trim())
    } else {
      failures.push(`Unable to read the active developer directory: ${commandFailure(selected)}`)
    }
    if (!candidates.includes(defaultDeveloperDirectory)) candidates.push(defaultDeveloperDirectory)
  }

  for (const directory of candidates) {
    const env = { ...environment, DEVELOPER_DIR: directory }
    const xcode = execute('xcodebuild', ['-version'], env)
    if (xcode.error || xcode.status !== 0) {
      failures.push(`${directory}: ${commandFailure(xcode)}`)
      continue
    }
    const version = xcode.stdout?.match(/^Xcode (\d+(?:\.\d+)*)/m)?.[1]
    if (!version || Number.parseInt(version, 10) < 26) {
      failures.push(`${directory}: unsupported Xcode version: ${xcode.stdout?.trim() || '(empty output)'}`)
      continue
    }
    const actool = execute('actool', ['--version'], env)
    if (!actool.error && actool.status === 0 && actool.stdout?.trim()) return env
    const diagnostic = !actool.error && actool.status === 0
      ? 'actool returned no version information'
      : commandFailure(actool)
    failures.push(`${directory}: unable to run actool: ${diagnostic}`)
  }

  throw new Error([
    'macOS desktop packaging requires full Xcode 26 or newer with actool. Command Line Tools alone are not sufficient.',
    ...failures,
    'Install Xcode and complete its first-launch setup, then retry.',
    'Set DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer for a specific installation; no global xcode-select change is needed.',
    'See docs/ci-cd.md for macOS packaging setup.',
  ].join('\n'))
}

if (require.main === module) {
  try {
    console.log(macOSPackagingEnvironment().DEVELOPER_DIR)
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}

module.exports = { macOSPackagingEnvironment }
