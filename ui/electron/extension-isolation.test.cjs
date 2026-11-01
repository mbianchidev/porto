const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')
const vm = require('node:vm')

test('preload never exposes desktop capabilities to extension subframes', () => {
  const source = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8')
  for (const isMainFrame of [false, true]) {
    const exposed = []
    vm.runInNewContext(source, {
      process: { isMainFrame },
      location: { origin: 'http://127.0.0.1:37623', pathname: '/' },
      require(name) {
        assert.equal(name, 'electron')
        return { contextBridge: { exposeInMainWorld: (name) => exposed.push(name) }, ipcRenderer: {} }
      },
    })
    assert.deepEqual(exposed, isMainFrame ? ['portoDesktop'] : [])
  }
})

test('IPC requires the real main dashboard frame, not just its URL', () => {
  const source = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8')
  const match = source.match(/function assertTrustedDashboardRequest\(event\) \{[\s\S]*?\n\}/)
  assert.ok(match)
  const check = vm.runInNewContext(`(${match[0]})`, { URL, DAEMON_URL: 'http://127.0.0.1:37623' })
  const mainFrame = { url: 'http://127.0.0.1:37623/#/extensions' }
  const sender = { mainFrame }
  assert.doesNotThrow(() => check({ senderFrame: mainFrame, sender }))
  assert.throws(() => check({ senderFrame: { url: mainFrame.url }, sender }), /only to the Porto dashboard/)
  assert.throws(() => check({ senderFrame: { url: 'https://synthetic.invalid' }, sender }), /only to the Porto dashboard/)
  const extensionFrame = { url: 'http://127.0.0.1:37623/api/extensions/dev.synthetic/ui' }
  assert.throws(() => check({ senderFrame: extensionFrame, sender: { mainFrame: extensionFrame } }), /only to the Porto dashboard/)
})
