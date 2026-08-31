const assert = require('node:assert/strict')
const test = require('node:test')

const { isDaemonReady } = require('./daemon-readiness.cjs')

function response(body, ok = true) {
  return {
    ok,
    async json() {
      return body
    },
  }
}

test('accepts a compatible daemon with a dashboard', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 2, dashboardReady: true }),
  })

  assert.equal(ready, true)
})

test('rejects an older daemon without dashboard readiness metadata', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 2 }),
  })

  assert.equal(ready, false)
})

test('rejects a daemon that cannot serve the dashboard', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 2, dashboardReady: false }),
  })

  assert.equal(ready, false)
})

test('rejects an incompatible API', async () => {
  const ready = await isDaemonReady({
    fetchImpl: async () => response({ status: 'ok', apiVersion: 1, dashboardReady: true }),
  })

  assert.equal(ready, false)
})
