const { spawnSync } = require('node:child_process')
const { setTimeout: delay } = require('node:timers/promises')

function transientDownloadFailure(output) {
  if (/checksum mismatch|SECURITY ERROR|unknown revision|invalid version|inconsistent vendoring|updates to go\.mod needed|context canceled|context deadline exceeded|\b(?:401 Unauthorized|403 Forbidden|404 Not Found)\b/i.test(output)) {
    return false
  }
  if (!/https?:\/\//.test(output)) return false
  return /stream error: stream ID \d+; (?:INTERNAL_ERROR|REFUSED_STREAM)|TLS handshake timeout|i\/o timeout|connection reset by peer|connection refused|unexpected EOF|no such host|temporary failure in name resolution|server misbehaving|\b(?:429 Too Many Requests|500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Timeout)\b/i.test(output)
}

async function runGoDependencies(args, {
  runImpl = spawnSync, delayImpl = delay, stdout = process.stdout, stderr = process.stderr,
} = {}) {
  if (!(args[0] === 'mod' && args[1] === 'download') &&
      !(args[0] === 'list' && args.includes('-deps')) && args[0] !== 'get') {
    throw new Error('Only Go dependency acquisition is supported: mod download, get, or list -deps')
  }
  const attempts = 3
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const result = runImpl('go', args, {
      env: process.env, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
      maxBuffer: 16 * 1024 * 1024,
    })
    if (result.stderr) stderr.write(result.stderr)
    if (!result.error && !result.signal && result.status === 0) {
      if (result.stdout) stdout.write(result.stdout)
      return 0
    }
    if (result.stdout) stderr.write(result.stdout)
    if (result.error || result.signal) {
      stderr.write(`Go dependency acquisition failed: ${result.error?.message || result.signal}\n`)
      return 1
    }
    if (attempt === attempts || !transientDownloadFailure(`${result.stderr || ''}\n${result.stdout || ''}`)) {
      return result.status || 1
    }
    const backoff = attempt * 2000
    stderr.write(`Warning: transient Go dependency transport failure (attempt ${attempt}/${attempts}); retrying in ${backoff / 1000}s.\n`)
    await delayImpl(backoff)
  }
}

if (require.main === module) {
  runGoDependencies(process.argv.slice(2)).then((status) => {
    process.exitCode = status
  }).catch((error) => {
    console.error(`Go dependency acquisition failed: ${error.message}`)
    process.exitCode = 1
  })
}

module.exports = { runGoDependencies }
