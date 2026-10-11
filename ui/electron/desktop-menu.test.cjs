const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')
const vm = require('node:vm')

function menuFixture(template) {
  const items = template.map((item) => ({
    ...item,
    submenu: item.submenu ? menuFixture(item.submenu) : undefined,
  }))
  return {
    items,
    getMenuItemById(id) {
      for (const item of items) {
        if (item.id === id) return item
        const nested = item.submenu?.getMenuItemById(id)
        if (nested) return nested
      }
      return null
    },
  }
}

function desktopFixture({ platform = 'darwin', phase = 'idle', resultPhase = 'up-to-date', checkError = null } = {}) {
  const calls = { checks: 0, dialogs: [], errors: [], shown: 0, applicationMenu: null, trayMenu: null }
  let status = {
    phase, currentVersion: '1.2.3', availableVersion: '', progressPercent: null,
    releaseURL: '', message: 'Synthetic initial update status.',
  }
  const context = vm.createContext({
    __dirname,
    console: { ...console, error: (...args) => calls.errors.push(args) },
    process: { platform, title: '', env: {} },
    URL,
    setImmediate,
    setInterval,
    clearInterval,
    setTimeout,
    require: (name) => name === 'electron' ? {
      app: {
        setName() {}, requestSingleInstanceLock: () => true, on() {},
        whenReady: () => new Promise(() => {}),
        focus() {}, dock: { show() {} },
      },
      dialog: {
        showMessageBox: async (_window, options) => {
          calls.dialogs.push(options)
          return { response: 1 }
        },
        showErrorBox: (title, message) => calls.errors.push([title, message]),
      },
      Menu: {
        buildFromTemplate: menuFixture,
        setApplicationMenu: (menu) => { calls.applicationMenu = menu },
        getApplicationMenu: () => calls.applicationMenu,
      },
      nativeImage: {
        createFromPath: () => ({ resize: () => ({ isEmpty: () => false }) }),
      },
      Tray: class {
        setToolTip() {}
        setContextMenu(menu) { calls.trayMenu = menu }
        on() {}
        destroy() { calls.trayMenu = null }
      },
    } : require(name),
  })
  vm.runInContext(fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8'), context)
  context.fixtureUpdater = {
    getStatus: () => ({ ...status }),
    check: async () => {
      calls.checks++
      if (checkError) throw checkError
      status = {
        ...status,
        phase: resultPhase,
        availableVersion: ['available', 'downloaded'].includes(resultPhase) ? '1.2.4' : '',
        message: resultPhase === 'up-to-date' ? 'Porto 1.2.3 is up to date.' : 'Synthetic update result.',
      }
      context.handleDesktopUpdateStatus(status)
      return { ...status }
    },
    download: async () => {
      status = { ...status, phase: 'downloading', message: 'Synthetic automatic download.' }
      context.handleDesktopUpdateStatus(status)
      await Promise.resolve()
      status = { ...status, phase: 'downloaded', message: 'Synthetic update ready to install.' }
      context.handleDesktopUpdateStatus(status)
      return { ...status }
    },
  }

  context.fixtureWindow = {
    isDestroyed: () => false, isVisible: () => true, isMinimized: () => false,
    show: () => { calls.shown++ }, focus() {}, moveTop() {},
    webContents: { send() {} },
  }
  vm.runInContext('desktopUpdater = fixtureUpdater; windows.add(fixtureWindow)', context)
  return {
    calls,
    context,
    publish(phase) {
      status = { ...status, phase }
      context.handleDesktopUpdateStatus(status)
    },
  }
}

test('update menus track busy and unsupported states', () => {
  const { calls, context, publish } = desktopFixture()
  context.configureApplicationMenu()
  context.configureTray(true)
  for (const phase of ['unsupported', 'checking', 'downloading', 'installing', 'error', 'up-to-date']) {
    publish(phase)
    for (const menu of [calls.applicationMenu, calls.trayMenu]) {
      const item = menu.getMenuItemById('porto-check-for-updates')
      assert.equal(item.enabled, ['error', 'up-to-date'].includes(phase), phase)
      assert.equal(item.label, phase === 'checking' ? 'Checking for updates...' : 'Check for updates...')
    }
  }
})

test('manual menu check errors are visible and allow retry', async () => {
  const { calls, context } = desktopFixture({ checkError: new Error('Synthetic update service outage.') })
  context.configureApplicationMenu()
  await calls.applicationMenu.getMenuItemById('porto-check-for-updates').click()
  assert.equal(calls.checks, 1)
  assert.equal(calls.dialogs.length, 0)
  assert.ok(calls.errors.some(([title, message]) => (
    title === 'Porto update check failed' && message === 'Synthetic update service outage.'
  )))
  assert.equal(calls.applicationMenu.getMenuItemById('porto-check-for-updates').enabled, true)
})

for (const phase of ['available', 'downloaded']) {
  test(`manual checks reopen a dismissed ${phase} prompt exactly once`, async () => {
    const { calls, context } = desktopFixture({ resultPhase: phase })
    context.configureApplicationMenu()
    const item = calls.applicationMenu.getMenuItemById('porto-check-for-updates')
    await item.click()
    await item.click()
    assert.equal(calls.checks, 2)
    assert.equal(calls.dialogs.length, 2)
    assert.equal(calls.dialogs[0].title, phase === 'available' ? 'Porto update available' : 'Porto update ready')
    if (phase === 'downloaded') assert.match(calls.dialogs[0].detail, /Synthetic update result/)
  })
}

test('repeated menu clicks do not duplicate an in-progress manual check', async () => {
  const { calls, context } = desktopFixture()
  context.configureApplicationMenu()
  context.configureTray(true)
  let release
  const gate = new Promise((resolve) => { release = resolve })
  const check = context.fixtureUpdater.check
  context.fixtureUpdater.check = async () => {
    await gate
    return check()
  }
  const helpCheck = calls.applicationMenu.getMenuItemById('porto-check-for-updates')
  const first = helpCheck.click()
  const second = calls.trayMenu.getMenuItemById('porto-check-for-updates').click()
  assert.equal(helpCheck.enabled, false)
  assert.equal(helpCheck.label, 'Checking for updates...')
  release()
  await Promise.all([first, second])
  assert.equal(calls.checks, 1)
  assert.equal(calls.dialogs.length, 1)
  assert.equal(helpCheck.enabled, true)
})

test('manual checks respect automatic downloads and show the current ready state', async () => {
  const { calls, context } = desktopFixture({ resultPhase: 'available' })
  vm.runInContext('desktopPreferences.automaticallyDownloadUpdates = true', context)
  context.configureApplicationMenu()
  await calls.applicationMenu.getMenuItemById('porto-check-for-updates').click()
  assert.equal(calls.dialogs.length, 1)
  assert.equal(calls.dialogs[0].title, 'Porto update ready')
  assert.match(calls.dialogs[0].detail, /Synthetic update ready to install/)
})

for (const platform of ['darwin', 'win32', 'linux']) {
  test(`${platform} Help and tray menus check for updates with visible results`, async () => {
    const { calls, context } = desktopFixture({ platform })
    context.configureApplicationMenu()
    context.configureTray(true)

    const help = calls.applicationMenu.items.find((item) => item.role === 'help')
    const helpCheck = help.submenu.getMenuItemById('porto-check-for-updates')
    const trayCheck = calls.trayMenu.getMenuItemById('porto-check-for-updates')
    assert.equal(helpCheck.label, 'Check for updates...')
    assert.equal(helpCheck.enabled, true)
    assert.equal(trayCheck.enabled, true)
    assert.ok(calls.applicationMenu.items.some((item) => item.role === 'editMenu'))
    assert.ok(calls.applicationMenu.items.some((item) => item.role === 'viewMenu'))
    assert.equal(calls.applicationMenu.items.some((item) => item.role === 'appMenu'), platform === 'darwin')

    await helpCheck.click()
    await trayCheck.click()
    assert.equal(calls.checks, 2)
    assert.equal(calls.dialogs.length, 2)
    assert.match(calls.dialogs[0].message, /Porto 1\.2\.3 is up to date/)
    assert.ok(calls.shown > 0)
  })
}
