const assert = require('node:assert/strict')
const { execFileSync, spawn, spawnSync } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { setTimeout: delay } = require('node:timers/promises')

if (!process.argv[2]) {
  throw new Error('Usage: node scripts/lima-runtime-smoke.cjs <limactl executable>')
}
const limactl = path.resolve(process.argv[2])
const instanceName = 'synthetic-pid-check'
const diskContents = 'Synthetic VM data must survive process-state recovery.\n'

function fixture(t) {
  const home = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-lima-pid-'))
  t.after(() => fs.rmSync(home, { recursive: true, force: true }))
  const directory = path.join(home, instanceName)
  fs.mkdirSync(directory)
  const config = [
    'vmType: qemu',
    'arch: default',
    'images:',
    '  - location: https://example.invalid/synthetic-lima-image.qcow2',
    'ssh:',
    '  localPort: 60022',
    'mounts: []',
    'containerd:',
    '  system: false',
    '  user: false',
    '',
  ].join('\n')
  fs.writeFileSync(path.join(directory, 'lima.yaml'), config)
  fs.writeFileSync(path.join(directory, 'diffdisk'), diskContents)
  const environment = Object.fromEntries(Object.entries(process.env)
    .filter(([key]) => key.toUpperCase() !== 'LIMA_HOME'))
  environment.LIMA_HOME = home
  const run = (args, overrides = {}) => {
    const inherited = Object.fromEntries(Object.entries(environment)
      .filter(([key]) => !Object.keys(overrides)
        .some((override) => key.toUpperCase() === override.toUpperCase())))
    return execFileSync(limactl, args, {
      cwd: home,
      env: { ...inherited, ...overrides },
      encoding: 'utf8',
      timeout: 30000,
      maxBuffer: 1024 * 1024,
      windowsHide: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    })
  }

  return {
    directory,
    run,
    inspect() {
      const output = run(['list', '--json', instanceName])
      const instances = output.trim().split(/\r?\n/).filter(Boolean).flatMap((line) => JSON.parse(line))
      assert.equal(instances.length, 1, output)
      assert.equal(instances[0].name, instanceName)
      assert.equal(fs.readFileSync(path.join(directory, 'lima.yaml'), 'utf8'), config)
      assert.equal(fs.readFileSync(path.join(directory, 'diffdisk'), 'utf8'), diskContents)
      return instances[0]
    },
  }
}

test('dead host-agent and QEMU PIDs recover to Stopped without changing VM data', (t) => {
  const exited = spawnSync(process.execPath, ['-e', ''], {
    timeout: 10000,
    windowsHide: true,
    stdio: 'pipe',
  })
  assert.ifError(exited.error)
  assert.equal(exited.status, 0)
  assert.ok(exited.pid > 0)
  const vm = fixture(t)
  for (const name of ['ha.pid', 'qemu.pid']) {
    fs.writeFileSync(path.join(vm.directory, name), `${exited.pid}\n`)
  }

  const instance = vm.inspect()
  assert.equal(instance.status, 'Stopped', JSON.stringify(instance.errors))
  assert.ok(!instance.errors?.length, JSON.stringify(instance.errors))
  for (const name of ['ha.pid', 'qemu.pid']) {
    assert.equal(fs.existsSync(path.join(vm.directory, name)), false, `${name} was not removed`)
  }
  assert.equal(vm.inspect().status, 'Stopped')
})

test('live process IDs are never discarded as stale', (t) => {
  const vm = fixture(t)
  fs.writeFileSync(path.join(vm.directory, 'ha.pid'), `${process.pid}\n`)

  const instance = vm.inspect()
  assert.equal(instance.hostAgentPID, process.pid, JSON.stringify(instance))
  assert.notEqual(instance.status, 'Stopped')
  assert.equal(fs.readFileSync(path.join(vm.directory, 'ha.pid'), 'utf8'), `${process.pid}\n`)
})

test('malformed PID files remain visible as errors instead of being deleted', (t) => {
  const vm = fixture(t)
  fs.writeFileSync(path.join(vm.directory, 'ha.pid'), 'not-a-process-id\n')

  const instance = vm.inspect()
  assert.equal(instance.status, 'Broken', JSON.stringify(instance))
  assert.ok(instance.errors?.length, JSON.stringify(instance))
  assert.equal(fs.readFileSync(path.join(vm.directory, 'ha.pid'), 'utf8'), 'not-a-process-id\n')
})

function processAlive(pid) {
  try {
    process.kill(pid, 0)
    return true
  } catch (error) {
    if (error.code === 'ESRCH') return false
    throw error
  }
}

async function waitFor(check, message) {
  const deadline = Date.now() + 5000
  while (Date.now() < deadline) {
    if (check()) return
    await delay(25)
  }
  assert.fail(message)
}

test('Windows forced stop terminates consoleless descendants and releases logs', {
  skip: process.platform !== 'win32',
}, async (t) => {
  const vm = fixture(t)
  const childPIDPath = path.join(vm.directory, 'synthetic-child.pid')
  const stdoutPath = path.join(vm.directory, 'ha.stdout.log')
  const stderrPath = path.join(vm.directory, 'ha.stderr.log')
  let agent
  let childPIDs = []
  try {
    const stdout = fs.openSync(stdoutPath, 'w')
    const stderr = fs.openSync(stderrPath, 'w')
    try {
      agent = spawn(process.execPath, ['-e', `
        const fs = require('node:fs')
        const { spawn } = require('node:child_process')
        const children = [0, 1].map(() => spawn(process.execPath, ['-e', 'setInterval(() => {}, 1000)'], {
          windowsHide: true, stdio: 'inherit',
        }))
        for (const child of children) {
          child.on('error', (error) => { console.error(error); process.exit(1) })
        }
        children[0].on('exit', () => process.exit(0))
        fs.writeFileSync(process.argv[1] + '.tmp', JSON.stringify(children.map((child) => child.pid)))
        fs.renameSync(process.argv[1] + '.tmp', process.argv[1])
        setInterval(() => {}, 1000)
      `, childPIDPath], { windowsHide: true, stdio: ['ignore', stdout, stderr] })
    } finally {
      fs.closeSync(stdout)
      fs.closeSync(stderr)
    }
    await new Promise((resolve, reject) => {
      agent.once('spawn', resolve)
      agent.once('error', reject)
    })
    await waitFor(() => {
      if (!fs.existsSync(childPIDPath)) return false
      childPIDs = JSON.parse(fs.readFileSync(childPIDPath, 'utf8'))
        .filter((pid) => Number.isInteger(pid) && pid > 0)
      return childPIDs.length === 2
    }, 'synthetic host agent did not start its driver and SSH child')
    fs.writeFileSync(path.join(vm.directory, 'ha.pid'), `${agent.pid}\n`)
    fs.writeFileSync(path.join(vm.directory, 'qemu.pid'), `${childPIDs[0]}\n`)

    assert.throws(() => vm.run(['stop', '--force', instanceName], { PATH: vm.directory }), /taskkill/)
    assert.ok(processAlive(agent.pid), 'failed stop killed the host agent')
    assert.ok(childPIDs.every(processAlive), 'failed stop killed a descendant')
    assert.equal(fs.readFileSync(path.join(vm.directory, 'ha.pid'), 'utf8'), `${agent.pid}\n`)
    assert.equal(fs.readFileSync(path.join(vm.directory, 'qemu.pid'), 'utf8'), `${childPIDs[0]}\n`)

    vm.run(['stop', '--force', instanceName])
    await waitFor(() => !processAlive(agent.pid) && childPIDs.every((pid) => !processAlive(pid)),
      'forced stop left the host agent or its descendant running')
    for (const log of [stdoutPath, stderrPath]) {
      fs.unlinkSync(log)
      fs.writeFileSync(log, 'ready for the next startup\n')
    }
    assert.equal(vm.inspect().status, 'Stopped')
  } finally {
    for (const pid of [agent?.pid, ...childPIDs]) {
      if (!pid || !processAlive(pid)) continue
      const result = spawnSync('taskkill.exe', ['/PID', String(pid), '/T', '/F'], {
        windowsHide: true, timeout: 10000, encoding: 'utf8',
      })
      assert.ifError(result.error)
      assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`)
      await waitFor(() => !processAlive(pid), `synthetic process ${pid} did not exit`)
    }
  }
})
