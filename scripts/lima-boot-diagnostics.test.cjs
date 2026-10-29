const assert = require('node:assert/strict')
const net = require('node:net')
const test = require('node:test')
const { bootRecords, queryQMP } = require('./lima-boot-diagnostics.cjs')

test('retains early disk enumeration and later mount failures without terminal escapes', () => {
  const output = bootRecords([
    '[0.10] virtio_blk virtio0: [vda] synthetic disk',
    '[0.11] vda: vda1 vda13 vda15',
    'systemd-udevd.service: starting',
    'Finished systemd-hwdb-update.service',
    'Out of memory: Killed process 42 (synthetic worker)',
    'Cannot inspect SSH sessions before environment reset',
    'SSH sessions are still opening after 30 seconds; refusing unsafe environment reset',
    '/mnt/lima-cidata/boot.Linux/07-etc-environment.sh: not found',
    'Unrelated package progress',
    '\u001b[31mTimed out waiting for /dev/disk/by-label/BOOT\u001b[0m\r',
    'Reached target emergency.target',
  ].join('\n'))
  assert.match(output, /vda1 vda13 vda15/)
  assert.match(output, /systemd-udevd\.service/)
  assert.match(output, /systemd-hwdb-update\.service/)
  assert.match(output, /Out of memory/)
  assert.match(output, /Cannot inspect SSH sessions/)
  assert.match(output, /refusing unsafe environment reset/)
  assert.match(output, /07-etc-environment\.sh: not found/)
  assert.match(output, /Timed out.*BOOT/)
  assert.match(output, /emergency.target/)
  assert.doesNotMatch(output, /\u001b|Unrelated package/)
})

test('bounds device diagnostics while preserving the beginning and end', () => {
  const output = bootRecords(`vda: first\n${'udev: synthetic \u03b4\u{1f600} event\n'.repeat(100)}boot.mount: failed\n`, 256)
  assert.ok(Buffer.byteLength(output) <= 256)
  assert.match(output, /^vda: first/)
  assert.match(output, /truncated/)
  assert.match(output, /boot.mount: failed$/)
  assert.doesNotMatch(output, /\uFFFD/)
})

async function qmpFixture(t, handle) {
  const sockets = new Set()
  const commands = []
  const server = net.createServer((socket) => {
    sockets.add(socket)
    socket.on('close', () => sockets.delete(socket))
    socket.setEncoding('utf8')
    socket.write(`${JSON.stringify({ QMP: { version: { qemu: { major: 11 } } } })}\n`)
    let pending = ''
    socket.on('data', (chunk) => {
      pending += chunk
      let newline
      while ((newline = pending.indexOf('\n')) !== -1) {
        const request = JSON.parse(pending.slice(0, newline))
        pending = pending.slice(newline + 1)
        commands.push(request.execute)
        handle(socket, request)
      }
    })
  })
  t.after(() => new Promise((resolve) => {
    for (const socket of sockets) socket.destroy()
    server.close(resolve)
  }))
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  return { endpoint: { host: '127.0.0.1', port: server.address().port }, commands }
}

test('queries actual QMP replies using only read-only snapshot commands', async (t) => {
  const values = {
    qmp_capabilities: {},
    'query-status': { running: true, status: 'running' },
    'query-block': [{ device: 'boot-disk', inserted: { file: '/synthetic/disk', drv: 'qcow2' } }],
    'query-cpus-fast': [{ 'cpu-index': 0, 'thread-id': 123 }],
  }
  const { endpoint, commands } = await qmpFixture(t, (socket, request) => {
    assert.ok(Object.hasOwn(values, request.execute), `unexpected QMP command ${request.execute}`)
    const reply = `${JSON.stringify({ return: values[request.execute], id: request.id })}\r\n`
    socket.write(reply.slice(0, 7))
    socket.write(reply.slice(7))
  })
  assert.deepEqual(await queryQMP(endpoint), {
    status: values['query-status'], blocks: values['query-block'], cpus: values['query-cpus-fast'],
  })
  assert.deepEqual(commands, ['qmp_capabilities', 'query-status', 'query-block', 'query-cpus-fast'])
})

test('surfaces QMP errors rather than returning a successful empty snapshot', async (t) => {
  const { endpoint } = await qmpFixture(t, (socket, request) => {
    socket.end(`${JSON.stringify({ id: request.id, error: { class: 'GenericError', desc: 'synthetic failure' } })}\n`)
  })
  await assert.rejects(queryQMP(endpoint), /synthetic failure/)
})

test('bounds an unresponsive QMP monitor without stopping the VM', async (t) => {
  const { endpoint, commands } = await qmpFixture(t, () => {})
  await assert.rejects(queryQMP(endpoint, 50), /timed out/)
  assert.ok(commands.every((command) => command === 'qmp_capabilities'))
})
