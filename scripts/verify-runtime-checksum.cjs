const crypto = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')

function sha256(file) {
  return crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex')
}

function verify(expected, file) {
  const normalized = String(expected).trim().toLowerCase()
  const actual = sha256(file)
  if (normalized === '' || normalized !== actual) {
    throw new Error(`checksum mismatch for ${path.basename(file)}: expected ${normalized}, got ${actual}`)
  }
  return actual
}

if (require.main === module) {
  const [mode, ...args] = process.argv.slice(2)
  try {
    if (mode === '--hash' && args.length === 1) {
      process.stdout.write(sha256(args[0]))
    } else if (mode === '--verify' && args.length === 2) {
      verify(args[0], args[1])
    } else {
      throw new Error('usage: verify-runtime-checksum.cjs <--hash file|--verify expected file>')
    }
  } catch (error) {
    console.error(error.message)
    process.exitCode = 1
  }
}

module.exports = { sha256, verify }
