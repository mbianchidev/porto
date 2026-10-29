const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const nodeTest = require('node:test')

const test = process.platform === 'win32' ? nodeTest.skip : nodeTest
const script = path.join(__dirname, 'apply-runtime-patch.sh')

function command(file, args, options = {}) {
  const result = spawnSync(file, args, { encoding: 'utf8', timeout: 10000, shell: false, ...options })
  assert.ifError(result.error)
  return result
}

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-runtime-patch-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true }))
  const source = path.join(root, 'scratch with spaces', 'extracted source \u00e9')
  fs.mkdirSync(source, { recursive: true })
  const initialized = command('git', ['init', '--quiet', root])
  assert.equal(initialized.status, 0, initialized.stderr)
  fs.writeFileSync(path.join(root, 'value.txt'), 'before\n')
  fs.writeFileSync(path.join(source, 'value.txt'), 'before\n')
  const patch = path.join(root, 'change.diff')
  fs.writeFileSync(patch, 'diff --git a/value.txt b/value.txt\n--- a/value.txt\n+++ b/value.txt\n@@ -1 +1 @@\n-before\n+after\n')
  return { root, source, patch }
}

test('extracted runtime source patches apply even when TMPDIR is inside a checkout', (t) => {
  const { root, source, patch } = fixture(t)
  const result = command('bash', [script, source, patch])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(path.join(source, 'value.txt'), 'utf8'), 'after\n')
  assert.equal(fs.readFileSync(path.join(root, 'value.txt'), 'utf8'), 'before\n')
  assert.equal(fs.existsSync(path.join(root, '.git', 'index')), false)
})

test('explicit inherited Git context cannot redirect source patches to the parent repository', (t) => {
  const { root, source, patch } = fixture(t)
  const result = command('bash', [script, source, patch], {
    env: { ...process.env, GIT_DIR: path.join(root, '.git'), GIT_WORK_TREE: root, GIT_INDEX_FILE: path.join(root, 'unrelated-index') },
  })
  assert.equal(result.status, 0, result.stderr)
  assert.equal(fs.readFileSync(path.join(source, 'value.txt'), 'utf8'), 'after\n')
  assert.equal(fs.readFileSync(path.join(root, 'value.txt'), 'utf8'), 'before\n')
  assert.equal(fs.existsSync(path.join(root, 'unrelated-index')), false)
})

test('a missing or already-applied patch fails instead of producing an unpatched success', (t) => {
  const { source, patch } = fixture(t)
  assert.equal(command('bash', [script, source, patch]).status, 0)
  const repeated = command('bash', [script, source, patch])
  assert.notEqual(repeated.status, 0)
  assert.equal(fs.readFileSync(path.join(source, 'value.txt'), 'utf8'), 'after\n')
})

test('host autocrlf cannot rewrite patched Linux guest scripts to CRLF', (t) => {
  const { source, patch } = fixture(t)
  const scriptPatch = path.join(path.dirname(patch), 'guest.diff')
  fs.writeFileSync(scriptPatch, [
    'diff --git a/guest.sh b/guest.sh',
    'new file mode 100755',
    '--- /dev/null',
    '+++ b/guest.sh',
    '@@ -0,0 +1,2 @@',
    '+#!/bin/sh',
    '+printf "synthetic guest ready\\n"',
    '',
  ].join('\n'))
  const result = command('bash', [script, source, scriptPatch], {
    env: { ...process.env, GIT_CONFIG_COUNT: '2',
      GIT_CONFIG_KEY_0: 'core.autocrlf', GIT_CONFIG_VALUE_0: 'true',
      GIT_CONFIG_KEY_1: 'core.eol', GIT_CONFIG_VALUE_1: 'crlf' },
  })
  assert.equal(result.status, 0, result.stderr)
  const contents = fs.readFileSync(path.join(source, 'guest.sh'), 'utf8')
  assert.equal(contents, '#!/bin/sh\nprintf "synthetic guest ready\\n"\n')
  assert.ok(!contents.includes('\r'))
  const executed = command(path.join(source, 'guest.sh'), [])
  assert.equal(executed.status, 0, executed.stderr)
  assert.equal(executed.stdout, 'synthetic guest ready\n')
})
