const assert = require('node:assert/strict')
const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const { createRequire } = require('node:module')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const { prepareMacOSDevelopmentApp } = require('./macos-development-app.cjs')

test('macOS startup handles unavailable power notifications without a workaround', {
  skip: process.platform !== 'darwin',
}, async (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto startup check '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const environment = { ...process.env }
  delete environment.DYLD_INSERT_LIBRARIES
  delete environment.ELECTRON_RUN_AS_NODE
  delete environment.ELECTRON_FORCE_IS_PACKAGED
  const smokeArguments = ['--disable-breakpad', '-ApplePersistenceIgnoreState', 'YES']
  const failure = path.join(directory, 'power-unavailable.dylib')
  const unavailableSource = path.join(directory, 'power-unavailable.c')
  fs.writeFileSync(unavailableSource, `
    #include <IOKit/pwr_mgt/IOPMLib.h>
    #include <stdio.h>
    #include <stdlib.h>
    static io_connect_t unavailable(void *context, IONotificationPortRef *port,
        IOServiceInterestCallback callback, io_object_t *notifier) {
      (void)context;
      (void)callback;
      *port = NULL;
      *notifier = IO_OBJECT_NULL;
      fputs("PORTO_TEST_POWER_UNAVAILABLE\\n", stderr);
      return IO_OBJECT_NULL;
    }
    __attribute__((constructor))
    static void confine_failure_injection(void) {
      if (unsetenv("DYLD_INSERT_LIBRARIES") != 0) {
        perror("Unable to confine the synthetic power-registration failure");
        abort();
      }
    }
    __attribute__((used, section("__DATA,__interpose")))
    static struct {
      io_connect_t (*replacement)(void *, IONotificationPortRef *, IOServiceInterestCallback, io_object_t *);
      io_connect_t (*original)(void *, IONotificationPortRef *, IOServiceInterestCallback, io_object_t *);
    } interpose = { unavailable, IORegisterForSystemPower };
  `)
  const compiled = spawnSync('xcrun', [
    'clang', '-Wall', '-Wextra', '-Werror', '-dynamiclib', '-arch', 'arm64', '-arch', 'x86_64',
    '-framework', 'IOKit', '-framework', 'CoreFoundation', unavailableSource, '-o', failure,
  ], { encoding: 'utf8', env: environment, timeout: 60000 })
  assert.ifError(compiled.error)
  assert.equal(compiled.status, 0, compiled.stderr)

  const repository = path.join(__dirname, '..')
  const requireElectron = createRequire(path.join(repository, 'ui', 'electron', 'package.json'))
  const electron = requireElectron('electron')
  const electronVersion = requireElectron('electron/package.json').version
  const application = path.join(directory, 'smoke.cjs')
  fs.writeFileSync(application, `
    const { app, BrowserWindow } = require('electron')
    const { realpathSync } = require('node:fs')
    let phase = 'waiting for app readiness'
    const timeout = setTimeout(() => {
      console.error('Timed out during ' + phase)
      app.exit(1)
    }, 30000)
    app.once('will-quit', () => {
      clearTimeout(timeout)
      console.log('PORTO_TEST_QUIT')
    })
    app.whenReady().then(async () => {
      if (process.env.PORTO_TEST_APP_PATH &&
          realpathSync(app.getAppPath()) !== realpathSync(process.env.PORTO_TEST_APP_PATH)) {
        throw new Error('The test must use only the synthetic application')
      }
      console.log('PORTO_TEST_INJECTOR_INHERITED=' + Boolean(process.env.DYLD_INSERT_LIBRARIES))
      console.log('PORTO_TEST_PACKAGED=' + app.isPackaged)
      console.log('PORTO_TEST_ELECTRON=' + process.versions.electron)
      phase = 'creating the sandboxed window'
      const window = new BrowserWindow({
        show: false,
        webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false },
      })
      phase = 'loading the synthetic page'
      await window.loadURL('data:text/html,<main>Synthetic Porto startup</main>')
      phase = 'executing the renderer script'
      const rendered = await window.webContents.executeJavaScript('document.querySelector("main")?.textContent')
      if (rendered !== 'Synthetic Porto startup') throw new Error('Renderer did not load')
      console.log('PORTO_TEST_RENDERED')
      phase = 'shutting down'
      window.destroy()
      app.quit()
    }).catch((error) => {
      console.error(error)
      app.exit(1)
    })
  `)

  function assertRendered(result, packaged, injectFailure) {
    assert.equal(result.error, undefined, `${result.error?.message}\n${result.stdout}\n${result.stderr}`)
    assert.equal(result.status, 0, `Electron startup failed (${result.signal}):\n${result.stdout}\n${result.stderr}`)
    if (injectFailure) {
      assert.match(result.stderr, /PORTO_TEST_POWER_UNAVAILABLE/, 'Failure injection was not exercised')
    }
    assert.match(result.stdout, /PORTO_TEST_RENDERED/)
    assert.match(result.stdout, /PORTO_TEST_QUIT/)
    assert.match(result.stdout, /PORTO_TEST_INJECTOR_INHERITED=false/)
    assert.ok(result.stdout.includes(`PORTO_TEST_PACKAGED=${packaged}`))
    assert.ok(result.stdout.includes(`PORTO_TEST_ELECTRON=${electronVersion}`),
      'The test must exercise the locked Electron runtime')
  }

  function assertNoWorkaround(bundle, executableName) {
    assert.equal(fs.existsSync(path.join(bundle, 'Contents', 'Frameworks', 'libporto-power-notification.dylib')), false)
    const links = spawnSync('xcrun', ['otool', '-L', path.join(bundle, 'Contents', 'MacOS', executableName)], {
      encoding: 'utf8', timeout: 10000,
    })
    assert.ifError(links.error)
    assert.equal(links.status, 0, links.stderr)
    assert.doesNotMatch(links.stdout, /libporto-power-notification/)
  }

  for (const injectFailure of [true, false]) {
    await t.test(injectFailure ? 'the unmodified Electron renders after failed power registration' : 'the unmodified Electron renders normally', () => {
      const result = spawnSync(electron, [
        application, `--user-data-dir=${path.join(directory, `runtime profile ${injectFailure}`)}`, ...smokeArguments,
      ], {
        encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
        env: { ...environment, ...(injectFailure ? { DYLD_INSERT_LIBRARIES: failure } : {}) },
      })
      assertRendered(result, false, injectFailure)
    })
  }

  await t.test('the development app renders and reopens after failed power registration without CLI app arguments', async () => {
    const source = path.join(directory, 'development source with spaces', 'ui', 'electron')
    fs.mkdirSync(source, { recursive: true })
    fs.writeFileSync(path.join(source, 'package.json'), JSON.stringify({
      name: 'synthetic-porto-dev', version: '1.0.0', main: 'main.cjs',
    }))
    fs.copyFileSync(application, path.join(source, 'main.cjs'))
    const bundle = await prepareMacOSDevelopmentApp({
      appDirectory: source, electronExecutable: electron, cacheRoot: path.join(directory, 'development cache'),
    })
    assertNoWorkaround(bundle, 'Electron')
    const executable = path.join(bundle, 'Contents', 'MacOS', 'Electron')
    for (const name of ['first launch', 'reopened']) {
      const result = spawnSync(executable, [
        `--user-data-dir=${path.join(directory, `development profile ${name}`)}`, ...smokeArguments,
      ], {
        encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
        env: { ...environment, PORTO_TEST_APP_PATH: source, DYLD_INSERT_LIBRARIES: failure },
      })
      assertRendered(result, false, true)
    }
    const node = spawnSync(executable, ['-e', 'console.log(process.execPath)'], {
      encoding: 'utf8', timeout: 10000,
      env: { ...environment, ELECTRON_RUN_AS_NODE: '1' },
    })
    assert.ifError(node.error)
    assert.equal(node.status, 0, node.stderr)
    assert.equal(fs.realpathSync(node.stdout.trim()), fs.realpathSync(executable))
  })

  await t.test('the packaged macOS app preserves Electron native startup without a workaround', async (t) => {
    const output = path.join(directory, 'packaged output')
    const bundle = path.join(output, `Porto-darwin-${process.arch}`, 'Porto.app')
    if (environment.PORTO_TEST_MACOS_APP) {
      fs.cpSync(environment.PORTO_TEST_MACOS_APP, bundle, { recursive: true, verbatimSymlinks: true })
    } else {
      const packageSource = path.join(directory, 'packaging source with spaces')
      const source = path.join(packageSource, 'ui', 'electron')
      fs.mkdirSync(source, { recursive: true })
      for (const file of ['package.cjs', 'desktop-updater.cjs', 'package.json', 'apply-update.ps1', 'apply-update.sh']) {
        fs.copyFileSync(path.join(repository, 'ui', 'electron', file), path.join(source, file))
      }
      fs.copyFileSync(application, path.join(source, 'main.js'))
      fs.cpSync(path.join(repository, 'ui', 'electron', 'assets'), path.join(source, 'assets'), { recursive: true })
      fs.symlinkSync(path.join(repository, 'scripts'), path.join(packageSource, 'scripts'), 'dir')
      fs.symlinkSync(path.join(repository, 'ui', 'electron', 'node_modules'), path.join(source, 'node_modules'), 'dir')
      fs.writeFileSync(path.join(source, 'package-fixture.txt'), 'Synthetic application resource')
      for (const target of ['darwin-arm64', 'darwin-x64', 'linux-x64', 'mas-arm64', 'win32-x64']) {
        const previousBuild = path.join(source, `Porto-${target}`)
        fs.mkdirSync(previousBuild)
        fs.writeFileSync(path.join(previousBuild, 'previous-build.txt'), 'Synthetic previous desktop build')
      }
      const packaged = spawnSync(process.execPath, [
        path.join(source, 'package.cjs'), '--platform=darwin', `--arch=${process.arch}`, `--out=${output}`,
      ], { encoding: 'utf8', env: environment, timeout: 120000 })
      assert.equal(packaged.error, undefined, `${packaged.error?.message}\n${packaged.stdout}\n${packaged.stderr}`)
      assert.equal(packaged.status, 0, `${packaged.stdout}\n${packaged.stderr}`)
    }
    assertNoWorkaround(bundle, 'Porto')
    const resources = path.join(bundle, 'Contents', 'Resources')
    await t.test('embeds modern and legacy Porto icons', () => {
      const info = requireElectron('plist').parse(fs.readFileSync(path.join(bundle, 'Contents', 'Info.plist'), 'utf8'))
      assert.equal(info.CFBundleIconName, 'Icon')
      assert.equal(info.CFBundleIconFile, 'porto.icns')
      assert.ok(fs.statSync(path.join(resources, 'Assets.car')).size > 0)
      assert.deepEqual(fs.readFileSync(path.join(resources, info.CFBundleIconFile)),
        fs.readFileSync(path.join(repository, 'ui', 'electron', 'assets', 'porto.icns')))
    })
    const archive = path.join(resources, 'app.asar')
    assert.equal(fs.statSync(archive).isFile(), true)
    await t.test('does not repackage previous desktop outputs when the destination changes', () => {
      const entries = requireElectron('@electron/asar').listPackage(archive)
      assert.equal(entries.some((entry) => /^\/Porto-(darwin|linux|mas|win32)-/.test(entry)), false,
        'Previous desktop bundles must not be copied into the application archive')
      if (!environment.PORTO_TEST_MACOS_APP) {
        assert.ok(entries.includes('/package-fixture.txt'), 'Ordinary application resources must remain packaged')
      }
    })
    fs.unlinkSync(archive)
    const fixture = path.join(resources, 'app')
    fs.mkdirSync(fixture)
    fs.writeFileSync(path.join(fixture, 'package.json'), JSON.stringify({
      name: 'synthetic-porto-startup', version: '1.0.0', main: 'main.cjs',
    }))
    fs.copyFileSync(application, path.join(fixture, 'main.cjs'))
    const executable = path.join(bundle, 'Contents', 'MacOS', 'Porto')
    const packagedEnvironment = { ...environment, PORTO_TEST_APP_PATH: fixture }
    for (const injectFailure of [true, false]) {
      await t.test(injectFailure ? 'renders and exits after failed power registration' : 'renders and exits with no injected environment', () => {
        const result = spawnSync(executable, [
          `--user-data-dir=${path.join(directory, `packaged profile ${injectFailure}`)}`, ...smokeArguments,
        ], {
          encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
          env: { ...packagedEnvironment, ...(injectFailure ? { DYLD_INSERT_LIBRARIES: failure } : {}) },
        })
        assertRendered(result, true, injectFailure)
      })
    }
    await t.test('preserves Electron Node mode and the executable path', () => {
      const result = spawnSync(executable, ['-e', 'console.log(process.execPath)'], {
        encoding: 'utf8', timeout: 10000,
        env: { ...packagedEnvironment, ELECTRON_RUN_AS_NODE: '1' },
      })
      assert.ifError(result.error)
      assert.equal(result.status, 0, result.stderr)
      assert.equal(fs.realpathSync(result.stdout.trim()), fs.realpathSync(executable))
    })
    await t.test('restores closed standard streams before starting Node', () => {
      const marker = path.join(directory, 'stdio restored')
      const result = spawnSync('/bin/sh', [
        '-c', 'exec "$@" 0<&- 1>&- 2>&-', 'porto-stdio-check', executable, '-e',
        'console.log("synthetic stdout"); console.error("synthetic stderr"); require("node:fs").writeFileSync(process.argv[1], "restored")',
        marker,
      ], {
        encoding: 'utf8', timeout: 10000,
        env: { ...packagedEnvironment, ELECTRON_RUN_AS_NODE: '1' },
      })
      assert.ifError(result.error)
      assert.equal(result.status, 0, result.stderr)
      assert.equal(fs.readFileSync(marker, 'utf8'), 'restored')
    })
  })
})
