const fs = require('node:fs')
const net = require('node:net')
const path = require('node:path')
const { stripVTControlCharacters } = require('node:util')

const diagnosticPattern = /\b(vda\d*|virtio|scsi|partition|udev(?:d|adm)?|hwdb|sysusers|tmpfiles|fsck|clocksource|tsc|io-apic|pit|watchdog|lockup|panic|rcu|oom)\b|modules-load|by-label|boot(?:-efi)?\.mount|local-fs|emergency|timed out|failed|stall|out of memory|killed process|allocation failure/i

function bootRecords(contents, limit = 65536) {
  const selected = stripVTControlCharacters(contents).replaceAll('\r', '\n')
    .split('\n').filter((line) => diagnosticPattern.test(line)).join('\n')
  const bytes = Buffer.from(selected)
  if (bytes.length <= limit) return selected || 'No kernel/device discovery records were found.'
  const marker = '\n... kernel/device records truncated ...\n'
  const half = Math.floor((limit - Buffer.byteLength(marker)) / 2)
  if (half < 1) throw new Error('Boot diagnostic output limit is too small')
  let beginning = half
  let ending = bytes.length - half
  while ((bytes[beginning] & 0xc0) === 0x80) beginning--
  while ((bytes[ending] & 0xc0) === 0x80) ending++
  return bytes.subarray(0, beginning).toString() + marker + bytes.subarray(ending).toString()
}

function queryQMP(endpoint, timeout = 5000) {
  return new Promise((resolve, reject) => {
    const socket = net.createConnection(endpoint)
    const requests = { status: 'query-status', blocks: 'query-block', cpus: 'query-cpus-fast' }
    const responses = new Map()
    let pending = ''
    let finished = false
    const timer = setTimeout(() => finish(new Error('QMP diagnostic request timed out')), timeout)
    function finish(error) {
      if (finished) return
      finished = true
      clearTimeout(timer)
      socket.destroy()
      if (error) reject(error)
      else resolve(Object.fromEntries(responses))
    }
    socket.setEncoding('utf8')
    socket.on('error', finish)
    socket.on('end', () => finish(new Error('QMP ended before the diagnostic snapshot completed')))
    socket.on('data', (chunk) => {
      pending += chunk
      if (Buffer.byteLength(pending) > 1024 * 1024) {
        finish(new Error('QMP diagnostic response exceeded its size limit'))
        return
      }
      let newline
      while (!finished && (newline = pending.indexOf('\n')) !== -1) {
        const line = pending.slice(0, newline)
        pending = pending.slice(newline + 1)
        let message
        try {
          message = JSON.parse(line)
        } catch (error) {
          finish(new Error('Invalid QMP diagnostic JSON', { cause: error }))
          return
        }
        if (!message || typeof message !== 'object' || Array.isArray(message)) {
          finish(new Error('Invalid QMP diagnostic message'))
        } else if (message.error) {
          finish(new Error(`QMP diagnostic ${message.id}: ${JSON.stringify(message.error)}`))
        } else if (message.QMP) {
          socket.write(`${JSON.stringify({ execute: 'qmp_capabilities', id: 'capabilities' })}\n`)
        } else if (message.id === 'capabilities') {
          for (const [id, execute] of Object.entries(requests)) {
            socket.write(`${JSON.stringify({ execute, id })}\n`)
          }
        } else if (Object.hasOwn(requests, message.id)) {
          if (!Object.hasOwn(message, 'return')) {
            finish(new Error(`QMP diagnostic ${message.id} omitted its result`))
            return
          }
          responses.set(message.id, message.return)
          if (responses.size === Object.keys(requests).length) finish()
        }
      }
    })
  })
}

async function collect(instance) {
  let failed = false
  console.error('\nCI Lima diagnostics: kernel and device discovery')
  try {
    console.error(bootRecords(fs.readFileSync(path.join(instance, 'serial.log'), 'utf8')))
  } catch (error) {
    console.error(`Unable to read kernel/device diagnostics: ${error.message}`)
    failed = true
  }
  if (process.platform !== 'win32') {
    console.error('\nCI Lima diagnostics: read-only QMP state, disk attachments and CPU metadata')
    try {
      console.error(JSON.stringify(await queryQMP(path.join(instance, 'qmp.sock')), null, 2))
    } catch (error) {
      console.error(`Unable to read QMP diagnostics: ${error.message}`)
      failed = true
    }
  } else {
    console.error('QMP socket diagnostics are unavailable for the Windows named-pipe transport.')
  }
  return failed ? 1 : 0
}

if (require.main === module) {
  if (process.argv.length !== 3) {
    console.error('usage: lima-boot-diagnostics.cjs <owned-instance-directory>')
    process.exitCode = 2
  } else {
    collect(process.argv[2]).then((code) => { process.exitCode = code }).catch((error) => {
      console.error(`Unable to collect Lima boot diagnostics: ${error.message}`)
      process.exitCode = 1
    })
  }
}

module.exports = { bootRecords, queryQMP }
