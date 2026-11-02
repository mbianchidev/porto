const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const test = require('node:test')

const {
  updateGoVersion,
  verifyReleaseVersions,
} = require('./release-version.cjs')

function writeJson(filePath, value) {
  fs.mkdirSync(path.dirname(filePath), { recursive: true })
  fs.writeFileSync(filePath, `${JSON.stringify(value, null, 2)}\n`)
}

function releaseFixture(t, version = '1.2.12') {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-release-version-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))

  fs.mkdirSync(path.join(root, 'internal/config'), { recursive: true })
  fs.writeFileSync(
    path.join(root, 'internal/config/config.go'),
    `package config\n\nvar Version = "${version}"\n`,
  )
  writeJson(path.join(root, 'ui/package.json'), { version })
  writeJson(path.join(root, 'ui/package-lock.json'), {
    version,
    packages: { '': { version } },
  })
  writeJson(path.join(root, 'ui/electron/package.json'), { version })
  writeJson(path.join(root, 'ui/electron/package-lock.json'), {
    version,
    packages: { '': { version } },
  })

  return root
}

test('updates the Go release version and verifies aligned metadata', (t) => {
  const root = releaseFixture(t)

  updateGoVersion(root, '1.2.13')
  for (const relativePath of [
    'ui/package.json',
    'ui/package-lock.json',
    'ui/electron/package.json',
    'ui/electron/package-lock.json',
  ]) {
    const filePath = path.join(root, relativePath)
    const contents = fs.readFileSync(filePath, 'utf8').replaceAll('1.2.12', '1.2.13')
    fs.writeFileSync(filePath, contents)
  }

  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.13'))
  assert.match(
    fs.readFileSync(path.join(root, 'internal/config/config.go'), 'utf8'),
    /^var Version = "1\.2\.13"$/m,
  )
})

test('reports the release file whose version does not match', (t) => {
  const root = releaseFixture(t)
  const packagePath = path.join(root, 'ui/electron/package.json')
  writeJson(packagePath, { version: '1.2.11' })

  assert.throws(
    () => verifyReleaseVersions(root, '1.2.12'),
    /ui\/electron\/package\.json has version "1\.2\.11"; expected 1\.2\.12/,
  )
})

test('rejects an unexpected Go version declaration', (t) => {
  const root = releaseFixture(t)
  fs.writeFileSync(path.join(root, 'internal/config/config.go'), 'package config\n')

  assert.throws(
    () => updateGoVersion(root, '1.2.13'),
    /does not contain the expected Version declaration/,
  )
})

test('release script avoids Bash heredoc pipe deadlocks', () => {
  const releaseScript = fs.readFileSync(path.join(__dirname, '..', 'release.sh'), 'utf8')

  assert.doesNotMatch(releaseScript, /<</)
})

function releaseScriptFixture(t) {
  const root = releaseFixture(t)
  const tools = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-release-tools-'))
  t.after(() => fs.rmSync(tools, { recursive: true, force: true }))
  const bin = path.join(tools, 'bin')
  const origin = path.join(tools, 'origin.git')
  const commandLog = path.join(tools, 'commands.log')
  const env = {
    PATH: process.env.PATH,
    HOME: tools,
    GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: path.join(tools, 'gitconfig'),
    GIT_TERMINAL_PROMPT: '0',
    LC_ALL: 'C',
  }
  fs.mkdirSync(bin)
  fs.writeFileSync(env.GIT_CONFIG_GLOBAL, [
    '[user]',
    '  name = Release Fixture',
    '  email = release-fixture@example.test',
    '[commit]',
    '  gpgSign = false',
    '[tag]',
    '  gpgSign = false',
    '[core]',
    '  autocrlf = false',
    '',
  ].join('\n'))
  fs.mkdirSync(path.join(root, 'scripts'))
  fs.copyFileSync(path.join(__dirname, '..', 'release.sh'), path.join(root, 'release.sh'))
  fs.copyFileSync(path.join(__dirname, 'release-version.cjs'), path.join(root, 'scripts/release-version.cjs'))
  for (const relativePath of [
    'ui/electron/apply-update.sh',
    'scripts/download-file.sh',
    'scripts/bundle-desktop-runtime.sh',
    'scripts/package-desktop-installer.sh',
    'scripts/install-desktop.sh',
  ]) {
    fs.writeFileSync(path.join(root, relativePath), '#!/bin/sh\nexit 0\n')
  }

  function git(...args) {
    const result = spawnSync('git', args, { cwd: root, env, encoding: 'utf8', timeout: 15000 })
    assert.ifError(result.error)
    assert.equal(result.status, 0, `git ${args.join(' ')}: ${result.stderr}`)
    return result.stdout.trim()
  }

  git('init', '--quiet', '--initial-branch=main')
  git('init', '--quiet', '--bare', '--initial-branch=main', origin)
  git('add', '.')
  git('commit', '--quiet', '--no-gpg-sign', '-m', 'Initial fixture')
  git('remote', 'add', 'origin', origin)
  git('push', '--quiet', '--set-upstream', 'origin', 'main')

  function executable(name, body) {
    fs.writeFileSync(path.join(bin, name), [
      '#!/bin/sh',
      'set -eu',
      `printf '%s\\t%s\\n' '${name}' "$*" >> "$FIXTURE_COMMAND_LOG"`,
      body,
      '',
    ].join('\n'), { mode: 0o700 })
  }

  executable('git', `
if [ "\${FIXTURE_PUSH_FAILURE:-0}" = 1 ] && [ "$1" = push ]; then
  printf 'synthetic atomic push failure\\n' >&2
  exit 42
fi
PATH="$FIXTURE_PATH" exec git "$@"`)
  executable('uname', 'printf "Darwin\\n"')
  executable('gofmt', '')
  executable('go', 'exit 0')
  executable('node', `
case "$1" in
  scripts/macos-toolchain.cjs) printf '%s\\n' "$FIXTURE_XCODE";;
  scripts/release-version.cjs) exec "$FIXTURE_NODE" "$@";;
  --check) exit 0;;
  *) printf 'unexpected node fixture command: %s\\n' "$*" >&2; exit 1;;
esac`)
  executable('npm', `
if [ "$1" = version ]; then
  exec "$FIXTURE_NODE" -e '
    const fs = require("node:fs")
    for (const file of ["package.json", "package-lock.json"]) {
      const value = JSON.parse(fs.readFileSync(file, "utf8"))
      value.version = process.argv[1]
      if (value.packages?.[""]) value.packages[""].version = process.argv[1]
      fs.writeFileSync(file, JSON.stringify(value, null, 2) + "\\n")
    }
  ' "$2"
fi
case "$*" in
  '--prefix ui ci'|'--prefix ui run lint'|'--prefix ui run build') ;;
  '--prefix ui/electron ci'|'--prefix ui/electron audit'|'--prefix ui/electron test') ;;
  *) printf 'unexpected npm fixture command: %s\\n' "$*" >&2; exit 1;;
esac`)

  function advanceOrigin(files) {
    const upstream = path.join(tools, 'upstream')
    git('clone', '--quiet', '--branch', 'main', origin, upstream)
    for (const [relativePath, contents] of Object.entries(files)) {
      fs.writeFileSync(path.join(upstream, relativePath), contents)
    }
    git('-C', upstream, 'add', '.')
    git('-C', upstream, 'commit', '--quiet', '--no-gpg-sign', '-m', 'Advance fixture main')
    git('-C', upstream, 'push', '--quiet', 'origin', 'main')
    return git('-C', upstream, 'rev-parse', 'HEAD')
  }

  function release(args, extraEnv = {}) {
    const result = spawnSync('bash', ['--noprofile', '--norc', path.join(root, 'release.sh'), ...args], {
      cwd: root,
      encoding: 'utf8',
      timeout: 30000,
      env: {
        ...env,
        PATH: `${bin}${path.delimiter}${env.PATH}`,
        FIXTURE_PATH: env.PATH,
        FIXTURE_COMMAND_LOG: commandLog,
        FIXTURE_NODE: process.execPath,
        FIXTURE_XCODE: path.join(tools, 'Xcode.app/Contents/Developer'),
        ...extraEnv,
      },
    })
    assert.ifError(result.error)
    return result
  }

  return {
    root, tools, git, release, advanceOrigin,
    commands: () => fs.existsSync(commandLog) ? fs.readFileSync(commandLog, 'utf8').trim().split('\n') : [],
  }
}

test('release publishes main and its annotated tag by default', { skip: process.platform === 'win32' }, (t) => {
  const { root, git, release, commands } = releaseScriptFixture(t)
  const result = release(['1.2.13'])

  assert.equal(result.status, 0, result.stderr)
  const head = git('rev-parse', 'HEAD')
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main').split('\t')[0], head)
  assert.equal(git('ls-remote', '--tags', 'origin', 'refs/tags/v1.2.13^{}').split('\t')[0], head)
  assert.equal(git('cat-file', '-t', 'refs/tags/v1.2.13'), 'tag')
  assert.equal(git('status', '--porcelain'), '')
  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.13'))
  assert.ok(commands().includes('git\tpush --atomic origin HEAD:refs/heads/main refs/tags/v1.2.13'))
})

test('release switches to main and pulls script updates before packaging or version changes', { skip: process.platform === 'win32' }, (t) => {
  const { root, git, release, commands, advanceOrigin } = releaseScriptFixture(t)
  git('switch', '--quiet', '--create', 'feature')
  const remoteMain = advanceOrigin({
    'release.sh': '#!/bin/sh\nexit 99\n',
    'pulled-main.txt': 'synthetic remote main\n',
  })

  const result = release(['1.2.13'])

  assert.equal(result.status, 0, result.stderr)
  assert.equal(git('branch', '--show-current'), 'main')
  assert.equal(git('rev-parse', 'HEAD^'), remoteMain)
  assert.equal(fs.readFileSync(path.join(root, 'pulled-main.txt'), 'utf8'), 'synthetic remote main\n')
  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.13'))
  const steps = commands()
  const switchStep = steps.indexOf('git\tswitch main')
  const pullStep = steps.indexOf('git\tpull --ff-only --tags origin main')
  assert.ok(switchStep >= 0)
  assert.ok(pullStep > switchStep)
  assert.ok(steps.indexOf('node\tscripts/macos-toolchain.cjs') > pullStep)
  assert.ok(steps.indexOf('node\tscripts/release-version.cjs update-go 1.2.13') > pullStep)
  assert.ok(steps.indexOf('gofmt\t-l .') > pullStep)
  assert.ok(steps.includes('git\tpush --atomic origin HEAD:refs/heads/main refs/tags/v1.2.13'))
})

test('only --mock performs a read-only trial, including when --push is present', { skip: process.platform === 'win32' }, (t) => {
  for (const args of [
    ['--mock', '1.2.13'],
    ['1.2.13', '--push', '--mock'],
    ['--mock', '--push', '1.2.13'],
  ]) {
    const { root, git, release, commands } = releaseScriptFixture(t)
    const refs = git('show-ref')
    const result = release(args)

    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stdout, /Mock run complete; would prepare and push main and v1\.2\.13 to origin/)
    assert.equal(git('show-ref'), refs)
    assert.equal(git('status', '--porcelain'), '')
    assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.12'))
    assert.ok(commands().includes('go\ttest ./... -count=1'))
    assert.ok(commands().includes('npm\t--prefix ui/electron test'))
    assert.ok(commands().every((step) => !/^git\t(switch|pull|fetch|add|commit|tag|push)( |$)/.test(step)))
  }

  const { release, commands } = releaseScriptFixture(t)
  const legacy = release(['--dry-run', '1.2.13'])
  assert.notEqual(legacy.status, 0)
  assert.match(legacy.stderr, /unknown option: --dry-run/)
  assert.deepEqual(commands(), [])
})

test('a failed default push can be retried without --push or another release commit', { skip: process.platform === 'win32' }, (t) => {
  const { git, release, commands } = releaseScriptFixture(t)
  const originalMain = git('rev-parse', 'HEAD')
  const failed = release(['1.2.13'], { FIXTURE_PUSH_FAILURE: '1' })

  assert.notEqual(failed.status, 0)
  assert.match(failed.stderr, /synthetic atomic push failure/)
  const releaseHead = git('rev-parse', 'HEAD')
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main').split('\t')[0], originalMain)
  assert.equal(git('cat-file', '-t', 'refs/tags/v1.2.13'), 'tag')
  assert.equal(git('status', '--porcelain'), '')

  const mockStart = commands().length
  const checked = release(['1.2.13', '--mock'])
  assert.equal(checked.status, 0, checked.stderr)
  assert.match(checked.stdout, /Mock run complete; v1\.2\.13 is ready to push/)
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main').split('\t')[0], originalMain)
  assert.ok(commands().slice(mockStart).every((step) => !/^git\t(switch|pull|fetch|add|commit|tag|push)( |$)/.test(step)))

  const retried = release(['1.2.13'])
  assert.equal(retried.status, 0, retried.stderr)
  assert.equal(git('rev-parse', 'HEAD'), releaseHead)
  assert.equal(git('ls-remote', 'origin', 'refs/heads/main').split('\t')[0], releaseHead)
  assert.equal(git('ls-remote', '--tags', 'origin', 'refs/tags/v1.2.13^{}').split('\t')[0], releaseHead)

  const published = release(['1.2.13', '--push'])
  assert.equal(published.status, 0, published.stderr)
  assert.match(published.stdout, /v1\.2\.13 is already published/)
  assert.equal(commands().filter((step) => step.startsWith('git\tcommit ')).length, 1)
  assert.equal(commands().filter((step) => step.startsWith('git\ttag ')).length, 1)
  assert.equal(commands().filter((step) => step.startsWith('git\tpush ')).length, 2)
})

test('dirty checkouts stop before switching, pulling, or preparing a release', { skip: process.platform === 'win32' }, (t) => {
  for (const relativePath of ['internal/config/config.go', 'untracked.txt']) {
    const { root, git, release, commands } = releaseScriptFixture(t)
    git('switch', '--quiet', '--create', 'feature')
    const refs = git('show-ref')
    const file = path.join(root, relativePath)
    fs.appendFileSync(file, 'synthetic local change\n')

    const result = release(['1.2.13'])

    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /working tree must be clean/)
    assert.equal(git('branch', '--show-current'), 'feature')
    assert.equal(git('show-ref'), refs)
    assert.match(fs.readFileSync(file, 'utf8'), /synthetic local change\n$/)
    assert.ok(commands().every((step) => !/^git\t(switch|pull|fetch|add|commit|tag|push)( |$)/.test(step)))
    assert.ok(commands().every((step) => step.startsWith('git\t')))
  }
})

test('diverged main stops before packaging, validation, or version changes', { skip: process.platform === 'win32' }, (t) => {
  const { root, git, release, commands, advanceOrigin } = releaseScriptFixture(t)
  fs.writeFileSync(path.join(root, 'local-main.txt'), 'synthetic local main\n')
  git('add', 'local-main.txt')
  git('commit', '--quiet', '--no-gpg-sign', '-m', 'Local fixture commit')
  const head = git('rev-parse', 'HEAD')
  advanceOrigin({ 'remote-main.txt': 'synthetic remote main\n' })

  const result = release(['1.2.13'])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /could not update main from origin\/main/)
  assert.equal(git('rev-parse', 'HEAD'), head)
  assert.equal(git('status', '--porcelain'), '')
  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.12'))
  assert.ok(commands().every((step) => step.startsWith('git\t')))
  assert.ok(commands().every((step) => !/^git\t(add|commit|tag|push)( |$)/.test(step)))
})

test('a main branch owned by another worktree is never taken over', { skip: process.platform === 'win32' }, (t) => {
  const { root, tools, git, release, commands } = releaseScriptFixture(t)
  git('switch', '--quiet', '--create', 'feature')
  git('worktree', 'add', '--quiet', path.join(tools, 'main-checkout'), 'main')
  const refs = git('show-ref')

  const result = release(['1.2.13'])

  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /could not switch to main.*another worktree/)
  assert.equal(git('branch', '--show-current'), 'feature')
  assert.equal(git('show-ref'), refs)
  assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.12'))
  assert.ok(commands().every((step) => step.startsWith('git\t')))
  assert.ok(commands().every((step) => !/^git\t(pull|fetch|add|commit|tag|push)( |$)/.test(step)))
})

test('mock trials refuse another branch or outdated main without updating Git', { skip: process.platform === 'win32' }, (t) => {
  for (const scenario of ['feature', 'outdated-main']) {
    const { root, git, release, commands, advanceOrigin } = releaseScriptFixture(t)
    if (scenario === 'feature') {
      git('switch', '--quiet', '--create', 'feature')
    } else {
      advanceOrigin({ 'remote-main.txt': 'synthetic remote main\n' })
      git('fetch', '--quiet', 'origin', 'main')
    }
    const refs = git('show-ref')

    const result = release(['--mock', '1.2.13'])

    assert.notEqual(result.status, 0)
    assert.match(result.stderr, /mock runs require main|main must match origin\/main/)
    assert.equal(git('show-ref'), refs)
    assert.equal(git('status', '--porcelain'), '')
    assert.doesNotThrow(() => verifyReleaseVersions(root, '1.2.12'))
    assert.ok(commands().every((step) => !/^git\t(switch|pull|fetch|add|commit|tag|push)( |$)/.test(step)))
    assert.ok(!commands().includes('gofmt\t-l .'))
  }
})
