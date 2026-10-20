const crypto = require('node:crypto')
const fs = require('node:fs')
const path = require('node:path')
const { spawnSync } = require('node:child_process')
const { Transform } = require('node:stream')
const { pipeline } = require('node:stream/promises')
const { createGunzip } = require('node:zlib')

const { verify } = require('./verify-runtime-checksum.cjs')

function selectArchive(info, goarch) {
  if (!['amd64', 'arm64'].includes(goarch)) throw new Error(`Unsupported CI guest architecture: ${goarch}`)
  const arch = goarch === 'amd64' ? 'x86_64' : 'aarch64'
  const archives = info?.defaultTemplate?.containerd?.archives
  if (!Array.isArray(archives)) throw new Error('Lima did not provide default containerd archives')
  const archive = archives.find((entry) => entry?.arch === arch)
  if (typeof archive?.location !== 'string' || !archive.location.trim() ||
      typeof archive.digest !== 'string' || !/^sha256:[a-f0-9]{64}$/i.test(archive.digest)) {
    throw new Error(`Lima did not provide a checksum-pinned containerd archive for ${arch}`)
  }
  return { location: archive.location, arch, digest: archive.digest }
}

async function decompressArchive(source, destination, expected) {
  verify(expected, source)
  const descriptor = fs.openSync(destination, 'wx', 0o600)
  const hash = crypto.createHash('sha256')
  try {
    await pipeline(
      fs.createReadStream(source),
      createGunzip(),
      new Transform({
        transform(chunk, _encoding, callback) {
          hash.update(chunk)
          callback(null, chunk)
        },
      }),
      fs.createWriteStream(destination, { fd: descriptor }),
    )
    return `sha256:${hash.digest('hex')}`
  } catch (error) {
    fs.rmSync(destination, { force: true })
    throw error
  }
}

function run(command, args, capture = false) {
  const result = spawnSync(command, args, {
    encoding: 'utf8', stdio: ['ignore', capture ? 'pipe' : 2, 2],
    timeout: capture ? 30000 : 360000, killSignal: 'SIGKILL',
  })
  if (result.error) throw new Error(`Unable to run ${command}: ${result.error.message}`, { cause: result.error })
  if (result.status !== 0) throw new Error(`${command} failed (${result.signal || result.status})`)
  return capture ? result.stdout.trim() : ''
}

function dependencyProvisionOverride() {
  const script = [
    '#!/bin/sh',
    'set -eu',
    "printf '%s\\n' 'Acquire::Languages \"none\";' 'Acquire::IndexTargets::deb::DEP-11::DefaultEnabled \"false\";' 'Acquire::IndexTargets::deb::CNF::DefaultEnabled \"false\";' > /etc/apt/apt.conf.d/99porto-ci",
    'rm -f /etc/apt/apt.conf.d/99needrestart',
    '',
  ].join('\n')
  return `.provision += ${JSON.stringify([{ mode: 'dependency', script }])}`
}

async function prepareContainerdArchive(goarch, directory) {
  const archive = selectArchive(JSON.parse(run('limactl', ['info'], true)), goarch)
  const destination = path.resolve(directory)
  fs.mkdirSync(destination, { mode: 0o700 })
  const compressed = path.join(destination, 'nerdctl-full.tar.gz')
  const uncompressed = path.join(destination, 'nerdctl-full.tar')
  console.error('Preparing checksum-verified containerd files on the host for software-emulated Lima.')
  run('bash', [path.join(__dirname, 'download-file.sh'), archive.location, compressed])
  const digest = await decompressArchive(compressed, uncompressed, archive.digest.slice('sha256:'.length))
  fs.unlinkSync(compressed)
  return `.containerd.archives = ${JSON.stringify([{ location: uncompressed, arch: archive.arch, digest }])}`
}

if (require.main === module) {
  const args = process.argv.slice(2)
  if (args.length === 1 && args[0] === '--dependencies') {
    process.stdout.write(`${dependencyProvisionOverride()}\n`)
  } else if (args.length !== 2) {
    console.error('usage: lima-containerd-archive.cjs <amd64|arm64> <new-directory> | --dependencies')
    process.exitCode = 2
  } else {
    prepareContainerdArchive(...args).then((expression) => {
      process.stdout.write(`${expression}\n`)
    }).catch((error) => {
      console.error(`Unable to prepare the CI containerd archive: ${error.message}`)
      process.exitCode = 1
    })
  }
}

module.exports = { selectArchive, decompressArchive, dependencyProvisionOverride }
