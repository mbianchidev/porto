const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const test = require('node:test')
const { spawn } = require('node:child_process')
const { once } = require('node:events')
const { setTimeout: delay } = require('node:timers/promises')

const { waitForUpdateHelperReady } = require('./desktop-update-install.cjs')

const packageJson = require('./package.json')

test('makes the Windows desktop shortcut an explicit installer choice', () => {
  assert.equal(packageJson.build.nsis.createDesktopShortcut, false)
  assert.equal(packageJson.build.nsis.include, 'assets/installer.nsh')
  assert.ok(packageJson.devDependencies['7zip-bin-full'])

  const installer = fs.readFileSync(path.join(__dirname, 'assets', 'installer.nsh'), 'utf8')
  assert.match(installer, /Create a desktop shortcut/)
  assert.match(installer, /\$\{BST_UNCHECKED\}/)
  assert.match(installer, /CreateShortCut "\$newDesktopLink"/)
  assert.match(installer, /\$\{IfNot\} \$\{isKeepShortcuts\}/)
})

test('preflights locked runtimes and marks silent installs as updates', () => {
  const updater = fs.readFileSync(path.join(__dirname, 'apply-update.ps1'), 'utf8')
  const blockerCheck = updater.indexOf('Stop running Porto VMs and Kubernetes clusters before updating')
  const readySignal = updater.indexOf('WriteAllText($ReadyFile')

  assert.ok(blockerCheck >= 0)
  assert.ok(readySignal > blockerCheck)
  assert.match(updater, /-ArgumentList "--updated \/S \/D=\$InstallDirectory"/)
})

test('Windows updater uses native executable and creation metadata without a CIM provider', () => {
  const updater = fs.readFileSync(path.join(__dirname, 'apply-update.ps1'), 'utf8')
  assert.doesNotMatch(updater, /Get-CimInstance|Win32_Process|ManagementObjectSearcher/)
  assert.match(updater, /MainModule\.FileName/)
  assert.match(updater, /StartTime\.ToUniversalTime\(\)\.Ticks/)
})

test('native Windows updater preflights cold file launches and unavailable CIM safely', {
  skip: process.platform !== 'win32',
}, async (t) => {
  const { environment, layout, prepare, systemExecutable, waitFor } = require('../../scripts/installed-fixture.cjs')
  for (const [mode, blocked] of [
    ['unavailable-cim', true], ['unavailable-cim', false],
    ['cold-file', true], ['cold-file', false],
  ]) {
    await t.test(`${mode}: ${blocked ? 'live QEMU remains untouched' : 'same-name unrelated process survives'}`, async () => {
      const directory = path.join(process.cwd(), `.installed-update-${process.pid}-${mode}-${blocked}`)
      const fixture = prepare(directory)
      const env = environment(fixture, layout(fixture.installRoot))
      const children = []
      const root = fixture.installRoot
      const executable = path.join(root, 'Porto.exe')
      const unrelated = path.join(directory, 'unrelated', 'Porto.exe')
      const packagePath = path.join(directory, 'synthetic-invalid-installer.exe')
      const installedHelper = path.join(root, 'resources', 'apply-update.ps1')
      const errorFile = path.join(directory, 'update.error')
      const readyFile = path.join(directory, 'update.ready')
      const proceedFile = path.join(directory, 'update.proceed')
      const literal = (value) => "'" + value.replaceAll("'", "''") + "'"
      const start = async (file) => {
        fs.mkdirSync(path.dirname(file), { recursive: true })
        fs.copyFileSync(process.execPath, file)
        const marker = file + '.started'
        const child = spawn(file, ['-e',
          'process.title="synthetic unrelated title";require("node:fs").writeFileSync(process.argv[1],"ready");setInterval(()=>{},1000)',
          marker], { env, windowsHide: true, stdio: 'ignore' })
        children.push(child)
        await once(child, 'spawn')
        await waitFor(() => fs.existsSync(marker), 'synthetic native executable must start', 10000)
        return child
      }
      let helper
      let diagnostics = ''
      try {
        const owned = await start(executable)
        const foreign = await start(unrelated)
        const qemu = blocked ? await start(path.join(root, 'runtime', 'qemu-system-synthetic.exe')) : null
        fs.copyFileSync(process.execPath, packagePath)
        fs.mkdirSync(path.dirname(installedHelper), { recursive: true })
        fs.copyFileSync(path.join(__dirname, 'apply-update.ps1'), installedHelper)
        const powershell = await systemExecutable('powershell.exe', env)
        const command = `
$ErrorActionPreference = 'Stop'
Import-Module Microsoft.PowerShell.Management, Microsoft.PowerShell.Utility
$PSModuleAutoLoadingPreference = 'None'
function Get-CimInstance { throw 'Synthetic CIM provider is unavailable' }
& ${literal(installedHelper)} -ParentPid 2147483647 -PackagePath ${literal(packagePath)} -InstallDirectory ${literal(root)} -ExecutablePath ${literal(executable)} -ErrorFile ${literal(errorFile)} -ReadyFile ${literal(readyFile)} -ProceedFile ${literal(proceedFile)} -SkipRestart
exit $LASTEXITCODE
`
        const invocation = mode === 'cold-file'
          ? ['-File', installedHelper, '-ParentPid', '2147483647', '-PackagePath', packagePath,
            '-InstallDirectory', root, '-ExecutablePath', executable, '-ErrorFile', errorFile,
            '-ReadyFile', readyFile, '-ProceedFile', proceedFile, '-SkipRestart']
          : ['-Command', command]
        helper = spawn(powershell, ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', ...invocation],
          { env, windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] })
        children.push(helper)
        for (const stream of [helper.stdout, helper.stderr]) {
          stream.on('data', (data) => { diagnostics = (diagnostics + data).slice(-16384) })
        }
        await once(helper, 'spawn')
        if (blocked) {
          await waitFor(() => helper.exitCode !== null, 'blocked native helper must exit', 15000)
          assert.notEqual(helper.exitCode, 0)
          assert.equal(fs.existsSync(readyFile), false)
          assert.match(fs.readFileSync(errorFile, 'utf8'), /Stop running Porto VMs and Kubernetes clusters/)
          assert.equal(qemu.exitCode, null)
          assert.equal(owned.exitCode, null)
        } else {
          await waitForUpdateHelperReady(helper, readyFile, { errorFile })
          assert.equal(owned.exitCode, null, 'readiness must not stop Porto before confirmation')
          fs.writeFileSync(proceedFile, 'proceed')
          await waitFor(() => helper.exitCode !== null, 'invalid synthetic installer must fail explicitly', 15000)
          assert.notEqual(helper.exitCode, 0)
          assert.match(fs.readFileSync(errorFile, 'utf8'), /Porto installer exited with code/)
          await waitFor(() => owned.exitCode !== null, 'only the owned executable must stop', 5000)
        }
        assert.equal(foreign.exitCode, null, 'unrelated same-name executable must survive')
      } catch (error) {
        const log = fs.existsSync(packagePath + '.install.log')
          ? fs.readFileSync(packagePath + '.install.log', 'utf8').slice(-16384)
          : 'Native helper did not create its startup log.'
        throw new Error(`Native updater fixture failed: ${error.message}\n${log}\n${diagnostics}`, { cause: error })
      } finally {
        for (const child of children) {
          if (!child.pid) continue
          if (child.exitCode !== null || child.signalCode !== null) continue
          const exited = once(child, 'exit')
          child.kill()
          await Promise.race([exited, delay(5000, undefined, { ref: false }).then(() => { throw new Error('Synthetic native child did not exit') })])
        }
        fs.rmSync(directory, { recursive: true, force: true })
      }
    })
  }
})
