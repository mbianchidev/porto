const assert = require('node:assert/strict')
const { spawnSync } = require('node:child_process')
const fs = require('node:fs')
const { createRequire } = require('node:module')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

const { installMacOSBootstrap } = require('../scripts/macos-desktop-bootstrap.cjs')
const { prepareMacOSDevelopmentApp } = require('../scripts/macos-development-app.cjs')

const launcher = path.join(__dirname, 'open-porto-macos-27.sh')
const guardSource = path.join(__dirname, 'macos-power-notification-compat.c')
const fallbackMessage = 'Porto: macOS power notifications are unavailable.'

test('the recovery launcher refuses non-macOS hosts before building anything', {
  skip: process.platform !== 'linux',
}, () => {
  const result = spawnSync('bash', [launcher], { encoding: 'utf8' })
  assert.ifError(result.error)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /only supports macOS 27/)
})

test('macOS packaging refuses to omit the native guard on other hosts', {
  skip: process.platform === 'darwin',
}, () => {
  assert.throws(() => installMacOSBootstrap('Synthetic Porto.app'), /on a macOS host/)
})

test('macOS power-notification workaround and upstream removal gate', {
  skip: process.platform !== 'darwin',
}, async (t) => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto power check '))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  const guard = path.join(directory, 'guard.dylib')
  const failure = path.join(directory, 'power-unavailable.dylib')
  const probe = path.join(directory, 'source-probe')
  const environment = { ...process.env }
  delete environment.DYLD_INSERT_LIBRARIES
  delete environment.ELECTRON_RUN_AS_NODE

  function compile(source, output, library = true) {
    const result = spawnSync('xcrun', [
      'clang', '-Wall', '-Wextra', '-Werror',
      ...(library ? ['-dynamiclib', '-arch', 'arm64', '-arch', 'x86_64'] : []),
      '-framework', 'IOKit', '-framework', 'CoreFoundation', source, '-o', output,
    ], { encoding: 'utf8', env: environment, timeout: 60000 })
    assert.ifError(result.error)
    assert.equal(result.status, 0, result.stderr)
  }

  compile(guardSource, guard)
  const unavailableSource = path.join(directory, 'power-unavailable.c')
  fs.writeFileSync(unavailableSource, `
    #include <IOKit/pwr_mgt/IOPMLib.h>
    #include <stdio.h>
    static io_connect_t unavailable(void *context, IONotificationPortRef *port,
        IOServiceInterestCallback callback, io_object_t *notifier) {
      (void)context;
      (void)callback;
      *port = NULL;
      *notifier = IO_OBJECT_NULL;
      fputs("PORTO_TEST_POWER_UNAVAILABLE\\n", stderr);
      return IO_OBJECT_NULL;
    }
    __attribute__((used, section("__DATA,__interpose")))
    static struct {
      io_connect_t (*replacement)(void *, IONotificationPortRef *, IOServiceInterestCallback, io_object_t *);
      io_connect_t (*original)(void *, IONotificationPortRef *, IOServiceInterestCallback, io_object_t *);
    } interpose = { unavailable, IORegisterForSystemPower };
  `)
  compile(unavailableSource, failure)

  const probeSource = path.join(directory, 'source-probe.c')
  fs.writeFileSync(probeSource, `
    #include <CoreFoundation/CoreFoundation.h>
    #include <IOKit/IOKitLib.h>
    #include <assert.h>
    #include <stdlib.h>
    int main(void) {
      assert(getenv("DYLD_INSERT_LIBRARIES") == NULL);
      IONotificationPortRef port = IONotificationPortCreate(kIOMainPortDefault);
      assert(port != NULL);
      CFRunLoopSourceRef valid = IONotificationPortGetRunLoopSource(port);
      CFRunLoopSourceRef missing = IONotificationPortGetRunLoopSource(NULL);
      assert(valid != NULL && missing != NULL && valid != missing);
      assert(CFRunLoopSourceIsValid(valid) && CFRunLoopSourceIsValid(missing));
      assert(IONotificationPortGetRunLoopSource(NULL) == missing);
      CFRunLoopAddSource(CFRunLoopGetCurrent(), missing, kCFRunLoopCommonModes);
      CFRunLoopRemoveSource(CFRunLoopGetCurrent(), missing, kCFRunLoopCommonModes);
      IONotificationPortDestroy(port);
      return 0;
    }
  `)
  compile(probeSource, probe, false)

  await t.test('valid ports pass through and null ports share one inert source', () => {
    const result = spawnSync(probe, [], {
      encoding: 'utf8', timeout: 10000,
      env: { ...environment, DYLD_INSERT_LIBRARIES: guard },
    })
    assert.ifError(result.error)
    assert.equal(result.status, 0, result.stderr)
    assert.equal(result.stderr.split(fallbackMessage).length - 1, 1)
  })

  const commands = path.join(directory, 'commands')
  const fakeApp = path.join(directory, 'Synthetic Porto.app')
  const openArguments = path.join(directory, 'open-arguments')
  fs.mkdirSync(commands)
  fs.mkdirSync(path.join(fakeApp, 'Contents', 'MacOS'), { recursive: true })
  fs.writeFileSync(path.join(fakeApp, 'Contents', 'MacOS', 'Porto'), '#!/bin/sh\nexit 0\n', { mode: 0o755 })
  fs.writeFileSync(path.join(commands, 'sw_vers'), '#!/bin/sh\nprintf "27.0\\n"\n', { mode: 0o755 })
  fs.writeFileSync(path.join(commands, 'pgrep'), '#!/bin/sh\nexit "${PORTO_TEST_PGREP_STATUS:-1}"\n', { mode: 0o755 })
  fs.writeFileSync(path.join(commands, 'open'), '#!/bin/sh\nprintf "%s\\n" "$@" > "$PORTO_TEST_OPEN_ARGS"\n', { mode: 0o755 })
  const launcherEnvironment = {
    ...environment,
    PATH: `${commands}${path.delimiter}${environment.PATH}`,
    HOME: path.join(directory, 'synthetic home'),
    PORTO_TEST_OPEN_ARGS: openArguments,
  }
  const cache = path.join(launcherEnvironment.HOME, 'Library', 'Caches', 'Porto', 'macos-power-notification')

  await t.test('the launcher builds a universal library and quotes paths with spaces', () => {
    const result = spawnSync('bash', [launcher, fakeApp], {
      encoding: 'utf8', env: launcherEnvironment, timeout: 60000,
    })
    assert.ifError(result.error)
    assert.equal(result.status, 0, result.stderr)
    const entries = fs.readdirSync(cache)
    assert.equal(entries.length, 1, 'Temporary build files were not cleaned up')
    const library = path.join(cache, entries[0])
    assert.equal(fs.readFileSync(openArguments, 'utf8'),
      `-a\n${fakeApp}\n--env\nDYLD_INSERT_LIBRARIES=${library}\n`)
    const architectures = spawnSync('xcrun', ['lipo', library, '-archs'], {
      encoding: 'utf8', timeout: 10000,
    })
    assert.ifError(architectures.error)
    assert.equal(architectures.status, 0, architectures.stderr)
    assert.deepEqual(architectures.stdout.trim().split(/\s+/).sort(), ['arm64', 'x86_64'])
  })

  await t.test('the launcher refuses an existing process and surfaces process-check errors', () => {
    fs.unlinkSync(openArguments)
    for (const status of ['0', '2']) {
      const result = spawnSync('bash', [launcher, fakeApp], {
        encoding: 'utf8', timeout: 10000,
        env: { ...launcherEnvironment, PORTO_TEST_PGREP_STATUS: status },
      })
      assert.ifError(result.error)
      assert.equal(result.status, status === '0' ? 1 : 2)
      assert.match(result.stderr, status === '0' ? /Quit the running Porto desktop/ : /Unable to inspect/)
      assert.equal(fs.existsSync(openArguments), false, 'The launcher must not launch another app')
    }
  })

  const requireElectron = createRequire(path.join(__dirname, '..', 'ui', 'electron', 'package.json'))
  const electron = requireElectron('electron')
  const electronVersion = requireElectron('electron/package.json').version
  const application = path.join(directory, 'smoke.cjs')
  fs.writeFileSync(application, `
    const { app, BrowserWindow } = require('electron')
    const { realpathSync } = require('node:fs')
    const timeout = setTimeout(() => {
      console.error('Timed out before rendering')
      app.exit(1)
    }, 30000)
    app.whenReady().then(async () => {
      if (process.env.PORTO_TEST_APP_PATH &&
          realpathSync(app.getAppPath()) !== realpathSync(process.env.PORTO_TEST_APP_PATH)) {
        throw new Error('The packaged test must use only the synthetic application')
      }
      console.log('PORTO_TEST_GUARD_INHERITED=' + Boolean(process.env.DYLD_INSERT_LIBRARIES))
      console.log('PORTO_TEST_PACKAGED=' + app.isPackaged)
      const window = new BrowserWindow({
        show: false,
        webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false },
      })
      await window.loadURL('data:text/html,<main>Synthetic Porto startup</main>')
      const rendered = await window.webContents.executeJavaScript('document.querySelector("main")?.textContent')
      if (rendered !== 'Synthetic Porto startup') throw new Error('Renderer did not load')
      console.log('PORTO_TEST_RENDERED')
      clearTimeout(timeout)
      window.destroy()
      app.quit()
    }).catch((error) => {
      console.error(error)
      app.exit(1)
    })
  `)

  function launch(name, libraries) {
    const result = spawnSync(electron, [
      application, `--user-data-dir=${path.join(directory, name)}`, '--disable-breakpad',
    ], {
      encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
      env: { ...environment, DYLD_INSERT_LIBRARIES: libraries },
    })
    assert.equal(result.error, undefined, `${result.error?.message}\n${result.stdout}\n${result.stderr}`)
    assert.match(result.stderr, /PORTO_TEST_POWER_UNAVAILABLE/, 'Failure injection was not exercised')
    return result
  }

  await t.test('Electron starts and renders when power registration fails', () => {
    const result = launch('guarded', `${failure}:${guard}`)
    assert.equal(result.status, 0, result.stderr)
    assert.match(result.stderr, /Continuing without sleep\/wake events/)
    assert.match(result.stdout, /PORTO_TEST_RENDERED/)
    assert.match(result.stdout, /PORTO_TEST_GUARD_INHERITED=false/)
  })

  await t.test('the locked Electron still needs the temporary guard', () => {
    const result = launch('unguarded', failure)
    assert.notEqual(result.status, 0,
      `Electron ${electronVersion} now handles failed power registration without the guard. ` +
      'Follow AGENTS.md and docs/installation.md: verify the upstream fix, remove the temporary macOS 27 helpers and packaged bootstrap, ' +
      'and retain this injected-failure case as a passing, unguarded regression test.')
    assert.equal(result.signal, 'SIGSEGV',
      `Unexpected unguarded startup failure; do not treat it as proof the shim is needed:\n${result.stderr}`)
  })

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
    const executable = path.join(bundle, 'Contents', 'MacOS', 'Electron')
    for (const name of ['first launch', 'reopened']) {
      const result = spawnSync(executable, [
        `--user-data-dir=${path.join(directory, `development profile ${name}`)}`, '--disable-breakpad',
      ], {
        encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
        env: { ...environment, PORTO_TEST_APP_PATH: source, DYLD_INSERT_LIBRARIES: failure },
      })
      assert.ifError(result.error)
      assert.equal(result.status, 0, `Guarded dev startup failed (${result.signal}):\n${result.stdout}\n${result.stderr}`)
      assert.match(result.stderr, /PORTO_TEST_POWER_UNAVAILABLE/, 'Failure injection was not exercised')
      assert.match(result.stderr, /Continuing without sleep\/wake events/)
      assert.match(result.stdout, /PORTO_TEST_RENDERED/)
      assert.match(result.stdout, /PORTO_TEST_PACKAGED=false/, 'Development must not enable packaged-only daemon replacement or updates')
      assert.match(result.stdout, /PORTO_TEST_GUARD_INHERITED=false/)
    }
    const node = spawnSync(executable, ['-e', 'console.log(process.execPath)'], {
      encoding: 'utf8', timeout: 10000,
      env: { ...environment, ELECTRON_RUN_AS_NODE: '1' },
    })
    assert.ifError(node.error)
    assert.equal(node.status, 0, node.stderr)
    assert.equal(fs.realpathSync(node.stdout.trim()), fs.realpathSync(executable))
  })

  await t.test('the packaged macOS app preserves native startup without a recovery launcher', async (t) => {
    const output = path.join(directory, 'packaged output')
    const bundle = path.join(output, `Porto-darwin-${process.arch}`, 'Porto.app')
    if (environment.PORTO_TEST_MACOS_APP) {
      fs.cpSync(environment.PORTO_TEST_MACOS_APP, bundle, { recursive: true, verbatimSymlinks: true })
    } else {
      const packaged = spawnSync(process.execPath, [
        path.join(__dirname, '..', 'ui', 'electron', 'package.cjs'),
        '--platform=darwin', `--arch=${process.arch}`, `--out=${output}`,
      ], { encoding: 'utf8', env: environment, timeout: 120000 })
      assert.ifError(packaged.error)
      assert.equal(packaged.status, 0, `${packaged.stdout}\n${packaged.stderr}`)
    }
    await t.test('accepts an already guarded app in a shared packaging directory', () => {
      assert.doesNotThrow(() => installMacOSBootstrap(bundle))
    })
    const resources = path.join(bundle, 'Contents', 'Resources')
    const archive = path.join(resources, 'app.asar')
    assert.equal(fs.statSync(archive).isFile(), true)
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
          `--user-data-dir=${path.join(directory, `packaged profile ${injectFailure}`)}`, '--disable-breakpad',
        ], {
          encoding: 'utf8', timeout: 45000, killSignal: 'SIGKILL',
          env: { ...packagedEnvironment, ...(injectFailure ? { DYLD_INSERT_LIBRARIES: failure } : {}) },
        })
        assert.equal(result.error, undefined, `${result.error?.message}\n${result.stdout}\n${result.stderr}`)
        if (injectFailure) {
          assert.match(result.stderr, /PORTO_TEST_POWER_UNAVAILABLE/, 'Failure injection was not exercised')
          assert.match(result.stderr, /Continuing without sleep\/wake events/)
        }
        assert.equal(result.status, 0, `Packaged startup failed (${result.signal}):\n${result.stderr}`)
        assert.match(result.stdout, /PORTO_TEST_RENDERED/)
        assert.match(result.stdout, /PORTO_TEST_PACKAGED=true/)
        assert.match(result.stdout, /PORTO_TEST_GUARD_INHERITED=false/)
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
