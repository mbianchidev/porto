const assert = require('node:assert/strict')
const test = require('node:test')

const { runGoDependencies } = require('./go-dependencies.cjs')

function fixture(results) {
  const calls = []
  const delays = []
  let stdout = ''
  let stderr = ''
  return {
    calls, delays,
    output: () => ({ stdout, stderr }),
    options: {
      runImpl: (command, args, options) => {
        calls.push({ command, args, options })
        assert.ok(results.length, 'Dependency acquisition exceeded the expected attempts')
        return results.shift()
      },
      delayImpl: async (duration) => { delays.push(duration) },
      stdout: { write: (text) => { stdout += text } },
      stderr: { write: (text) => { stderr += text } },
    },
  }
}

const success = { status: 0, stdout: 'synthetic/package\n', stderr: '' }
const transportFailure = {
  status: 1, stdout: 'partial/package\n',
  stderr: 'fixture.go:5:2: example.invalid/synthetic@v1.0.0: read "https://proxy.example.invalid/module.zip": stream error: stream ID 17; INTERNAL_ERROR; received from peer\n',
}

test('dependency acquisition forwards successful output once without changing Go arguments or environment', async () => {
  const state = fixture([success])
  const args = ['list', '-mod=mod', '-deps', '-f', '{{.ImportPath}}', './cmd/synthetic']
  assert.equal(await runGoDependencies(args, state.options), 0)
  assert.equal(state.calls.length, 1)
  assert.equal(state.calls[0].command, 'go')
  assert.deepEqual(state.calls[0].args, args)
  assert.equal(state.calls[0].options.env, process.env)
  assert.deepEqual(state.delays, [])
  assert.deepEqual(state.output(), { stdout: success.stdout, stderr: '' })
})

test('retries HTTP/2 module transport failures without duplicating partial import graphs', async () => {
  const state = fixture([transportFailure, transportFailure, success])
  assert.equal(await runGoDependencies(['list', '-deps', './cmd/synthetic'], state.options), 0)
  assert.equal(state.calls.length, 3)
  assert.deepEqual(state.delays, [2000, 4000])
  assert.equal(state.output().stdout, success.stdout)
  assert.match(state.output().stderr, /INTERNAL_ERROR/)
  assert.match(state.output().stderr, /attempt 1\/3/)
  assert.match(state.output().stderr, /attempt 2\/3/)
})

test('persistent transport failures remain failures after the bounded dependency retries', async () => {
  const state = fixture(Array(3).fill(transportFailure))
  assert.equal(await runGoDependencies(['mod', 'download'], state.options), 1)
  assert.equal(state.calls.length, 3)
  assert.deepEqual(state.delays, [2000, 4000])
  assert.equal(state.output().stdout, '')
  assert.match(state.output().stderr, /INTERNAL_ERROR/)
})

test('retries only recognized dependency transport and server failures', async (t) => {
  for (const diagnostic of [
    'Get "https://proxy.example.invalid/module.zip": net/http: TLS handshake timeout',
    'Get "https://proxy.example.invalid/module.zip": dial tcp: lookup proxy.example.invalid: no such host',
    'read "https://proxy.example.invalid/module.zip": connection reset by peer',
    'read "https://proxy.example.invalid/module.zip": unexpected EOF',
    'reading https://proxy.example.invalid/module.zip: 429 Too Many Requests',
    'reading https://proxy.example.invalid/module.zip: 502 Bad Gateway',
    'reading https://proxy.example.invalid/module.zip: 503 Service Unavailable',
  ]) {
    await t.test(diagnostic, async () => {
      const state = fixture([{ status: 1, stdout: '', stderr: diagnostic }, success])
      assert.equal(await runGoDependencies(['get', 'example.invalid/synthetic@v1.0.0'], state.options), 0)
      assert.equal(state.calls.length, 2)
    })
  }
})

test('does not retry integrity, version, authentication, source or unrecognized failures', async (t) => {
  for (const diagnostic of [
    'verifying example.invalid/synthetic@v1.0.0: checksum mismatch\n' + transportFailure.stderr,
    'SECURITY ERROR\n' + transportFailure.stderr,
    'example.invalid/synthetic@v1.0.0: unknown revision v1.0.0',
    'reading https://proxy.example.invalid/module.zip: 404 Not Found',
    'reading https://proxy.example.invalid/module.zip: 401 Unauthorized',
    'reading https://proxy.example.invalid/module.zip: 403 Forbidden',
    'fixture.go:2:1: expected declaration, found syntax',
    'go: updates to go.mod needed; to update it: go mod tidy',
    'go: inconsistent vendoring',
    'synthetic unrecognized acquisition failure',
  ]) {
    await t.test(diagnostic, async () => {
      const state = fixture([{ status: 2, stdout: '', stderr: diagnostic }])
      assert.equal(await runGoDependencies(['mod', 'download'], state.options), 2)
      assert.equal(state.calls.length, 1)
      assert.deepEqual(state.delays, [])
      assert.match(state.output().stderr, new RegExp(diagnostic.split('\n')[0].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')))
    })
  }
})

test('spawn failures and signals fail explicitly without another acquisition attempt', async (t) => {
  for (const result of [
    { status: null, error: Object.assign(new Error('spawn go ENOENT'), { code: 'ENOENT' }) },
    { status: null, signal: 'SIGTERM' },
  ]) {
    await t.test(result.error?.message || result.signal, async () => {
      const state = fixture([result])
      assert.equal(await runGoDependencies(['mod', 'download'], state.options), 1)
      assert.equal(state.calls.length, 1)
      assert.deepEqual(state.delays, [])
      assert.match(state.output().stderr, /ENOENT|SIGTERM/)
    })
  }
})

test('never retries or invokes builds, tests, module edits or unrelated commands', async () => {
  for (const args of [[], ['build', './...'], ['test', './...'], ['vet', './...'], ['mod', 'tidy'], ['mod', 'edit']]) {
    const state = fixture([])
    await assert.rejects(runGoDependencies(args, state.options), /dependency acquisition/)
    assert.deepEqual(state.calls, [])
  }
})
