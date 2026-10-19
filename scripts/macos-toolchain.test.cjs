const assert = require('node:assert/strict')
const test = require('node:test')

const { macOSDeveloperDirectory } = require('./macos-toolchain.cjs')

const defaultDeveloperDirectory = '/Applications/Xcode.app/Contents/Developer'
const commandLineTools = '/Library/Developer/CommandLineTools'
const selectedDeveloperDirectory = '/Synthetic Applications/Xcode Selected.app/Contents/Developer'

function tools({ selected = commandLineTools, selectionResult, installations = {} } = {}) {
  const calls = []
  const run = (command, args, options) => {
    calls.push({ command, args, options })
    assert.equal(options.encoding, 'utf8')
    assert.ok(options.timeout > 0)
    if (command === 'xcode-select') {
      assert.deepEqual(args, ['--print-path'])
      if (selectionResult) return selectionResult
      if (options.env.DEVELOPER_DIR) return { status: 0, stdout: `${options.env.DEVELOPER_DIR}\n` }
      return typeof selected === 'string' ? { status: 0, stdout: `${selected}\n` } : selected
    }
    const installation = installations[options.env.DEVELOPER_DIR]
    if (command === 'xcodebuild') {
      assert.deepEqual(args, ['-version'])
      return installation?.versionResult ?? (installation
        ? { status: 0, stdout: `Xcode ${installation.version ?? '26.0'}\nBuild version SYNTHETIC\n` }
        : { status: 1, stderr: 'Synthetic developer directory does not contain full Xcode' })
    }
    assert.equal(command, 'actool')
    assert.deepEqual(args, ['--version'])
    assert.ok(installation, 'actool must only be checked after validating Xcode')
    return installation.actoolResult ?? {
      status: 0, stdout: '<plist><dict><key>short-bundle-version</key><string>26.0</string></dict></plist>\n',
    }
  }
  return { run, calls }
}

test('returns only the selected Xcode directory without exposing or changing the parent environment', () => {
  const environment = { DEVELOPER_DIR: selectedDeveloperDirectory, PATH: '/synthetic/bin' }
  const original = { ...environment }
  const { run, calls } = tools({ installations: { [selectedDeveloperDirectory]: {} } })

  const resolved = macOSDeveloperDirectory(environment, run)

  assert.equal(resolved, selectedDeveloperDirectory)
  assert.deepEqual(environment, original)
  assert.deepEqual(calls.map(({ command }) => command), ['xcode-select', 'xcodebuild', 'actool'])
  for (const { options } of calls) {
    assert.deepEqual(options.env, original)
  }
})

test('preserves a suitable active Xcode installation', () => {
  const { run, calls } = tools({
    selected: selectedDeveloperDirectory,
    installations: { [selectedDeveloperDirectory]: { version: '27.1' } },
  })

  assert.equal(macOSDeveloperDirectory({}, run), selectedDeveloperDirectory)
  assert.deepEqual(calls.map(({ command }) => command), ['xcode-select', 'xcodebuild', 'actool'])
})

test('uses installed Xcode when Command Line Tools are selected, without switching globally', () => {
  const environment = { PATH: '/synthetic/bin' }
  const { run, calls } = tools({ installations: { [defaultDeveloperDirectory]: {} } })

  const resolved = macOSDeveloperDirectory(environment, run)

  assert.equal(resolved, defaultDeveloperDirectory)
  assert.deepEqual(environment, { PATH: '/synthetic/bin' })
  assert.deepEqual(calls.filter(({ command }) => command === 'xcode-select').map(({ args }) => args), [
    ['--print-path'],
  ])
  assert.deepEqual(calls.filter(({ command }) => command === 'xcodebuild')
    .map(({ options }) => options.env.DEVELOPER_DIR), [commandLineTools, defaultDeveloperDirectory])
})

test('falls back to installed Xcode when the active selection cannot be read', () => {
  const { run } = tools({
    selected: { error: new Error('Synthetic xcode-select failure') },
    installations: { [defaultDeveloperDirectory]: {} },
  })

  assert.equal(macOSDeveloperDirectory({}, run), defaultDeveloperDirectory)
})

test('reports missing full Xcode with setup instructions and command diagnostics', () => {
  const { run } = tools()

  assert.throws(() => macOSDeveloperDirectory({}, run), (error) => {
    assert.match(error.message, /Xcode 26 or newer/)
    assert.match(error.message, /Command Line Tools alone/)
    assert.match(error.message, /Synthetic developer directory does not contain full Xcode/)
    assert.ok(error.message.includes(commandLineTools))
    assert.match(error.message, /DEVELOPER_DIR=\/Applications\/Xcode\.app\/Contents\/Developer/)
    assert.match(error.message, /first.launch/i)
    return true
  })
})

test('does not silently replace an invalid explicit Xcode override', () => {
  const { run, calls } = tools({ installations: { [defaultDeveloperDirectory]: {} } })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Synthetic developer directory does not contain full Xcode/)
  assert.equal(calls.length, 2)
  assert.equal(calls[1].options.env.DEVELOPER_DIR, selectedDeveloperDirectory)
})

test('does not replace an explicit override when its developer directory cannot be queried', () => {
  const { run, calls } = tools({
    selectionResult: { status: 1, stderr: 'Synthetic explicit developer directory query failed' },
    installations: { [defaultDeveloperDirectory]: {} },
  })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Synthetic explicit developer directory query failed/)
  assert.equal(calls.length, 1)
})

test('rejects an Xcode version too old for Icon Composer', () => {
  const { run, calls } = tools({ installations: { [selectedDeveloperDirectory]: { version: '16.4' } } })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Xcode 16\.4/)
  assert.equal(calls.length, 2)
})

test('rejects an unrecognized Xcode version instead of assuming compatibility', () => {
  const { run } = tools({
    installations: {
      [selectedDeveloperDirectory]: { versionResult: { status: 0, stdout: 'Synthetic unknown toolchain' } },
    },
  })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Synthetic unknown toolchain/)
})

test('reports an unavailable asset compiler even with a supported Xcode version', () => {
  const { run } = tools({
    installations: {
      [selectedDeveloperDirectory]: { actoolResult: { status: 1, stderr: 'Synthetic actool unavailable' } },
    },
  })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Synthetic actool unavailable/)
})

test('preserves tool execution errors in the setup failure', () => {
  const { run } = tools({
    installations: {
      [selectedDeveloperDirectory]: { versionResult: { error: new Error('Synthetic tool timeout') } },
    },
  })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /Synthetic tool timeout/)
})

test('rejects an asset compiler that returns no version information', () => {
  const { run } = tools({
    installations: {
      [selectedDeveloperDirectory]: { actoolResult: { status: 0, stdout: '' } },
    },
  })

  assert.throws(() => macOSDeveloperDirectory({ DEVELOPER_DIR: selectedDeveloperDirectory }, run),
    /actool returned no version information/)
})

test('uses installed Xcode when the active Xcode is too old', () => {
  const { run } = tools({
    selected: selectedDeveloperDirectory,
    installations: {
      [selectedDeveloperDirectory]: { version: '16.4' },
      [defaultDeveloperDirectory]: {},
    },
  })

  assert.equal(macOSDeveloperDirectory({}, run), defaultDeveloperDirectory)
})
