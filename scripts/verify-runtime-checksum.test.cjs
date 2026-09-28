const assert = require('node:assert/strict')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const { sha256, verify } = require('./verify-runtime-checksum.cjs')

test('verifies the exact runtime artifact digest', (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-runtime-checksum-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const artifact = path.join(directory, 'artifact.bin')
  fs.writeFileSync(artifact, 'porto-runtime')
  const expected = sha256(artifact)

  assert.equal(verify(expected.toUpperCase(), artifact), expected)
})

test('rejects a runtime artifact checksum mismatch', (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-runtime-checksum-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const artifact = path.join(directory, 'artifact.bin')
  fs.writeFileSync(artifact, 'tampered-runtime')

  assert.throws(
    () => verify('0'.repeat(64), artifact),
    /checksum mismatch for artifact\.bin/,
  )
})
