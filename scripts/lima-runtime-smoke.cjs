const assert = require('node:assert/strict')
const { execFileSync, spawnSync } = require('node:child_process')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')

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

  return {
    directory,
    inspect() {
      const output = execFileSync(limactl, ['list', '--json', instanceName], {
        cwd: home,
        env: environment,
        encoding: 'utf8',
        timeout: 30000,
        maxBuffer: 1024 * 1024,
        windowsHide: true,
        stdio: ['ignore', 'pipe', 'pipe'],
      })
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
