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

function run(command, args, capture = false, env = process.env) {
  const result = spawnSync(command, args, {
    env, encoding: 'utf8', stdio: ['ignore', capture ? 'pipe' : 2, 2],
    timeout: capture ? 30000 : 360000, killSignal: 'SIGKILL',
  })
  if (result.error) throw new Error(`Unable to run ${command}: ${result.error.message}`, { cause: result.error })
  if (result.status !== 0) throw new Error(`${command} failed (${result.signal || result.status})`)
  return capture ? result.stdout.trim() : ''
}

function dependencyProvisionOverride(date = new Date()) {
  const completedDay = new Date(date.getTime() - 24 * 60 * 60 * 1000)
  const snapshot = `${completedDay.toISOString().slice(0, 10).replaceAll('-', '')}T000000Z`
  const script = [
    '#!/bin/sh',
    'set -eu',
    `printf '%s\\n' 'CI Ubuntu snapshot: ${snapshot}' >&2`,
    'sources=/etc/apt/sources.list.d/ubuntu.sources',
    'if ! grep -q "^URIs: " "$sources"; then echo "CI Ubuntu archive sources are unavailable" >&2; exit 1; fi',
    `sed -i '/^Snapshot:/d; s|^URIs:.*|URIs: https://snapshot.ubuntu.com/ubuntu/${snapshot}/|' "$sources"`,
    "cat > /etc/apt/apt.conf.d/99porto-ci <<'PORTO_APT_CONFIG'",
    'Acquire::Languages "none";',
    'Acquire::IndexTargets::deb::DEP-11::DefaultEnabled "false";',
    'Acquire::IndexTargets::deb::CNF::DefaultEnabled "false";',
    'Acquire::Retries "10";',
    'Acquire::Retries::Delay "true";',
    'Acquire::Retries::Delay::Maximum "30";',
    'Acquire::http::Timeout "30";',
    'Acquire::https::Timeout "30";',
    'Debug::Acquire::Retries "true";',
    'APT::Update::Error-Mode "any";',
    'PORTO_APT_CONFIG',
    'rm -f /etc/apt/apt.conf.d/99needrestart',
    '',
  ].join('\n')
  return `.provision += ${JSON.stringify([{ mode: 'dependency', script }])}`
}

function intelTCGImages() {
  const release = 'https://cloud-images.ubuntu.com/releases/resolute/release-20260927'
  const image = 'ubuntu-26.04-server-cloudimg-amd64'
  return [{
    location: `${release}/${image}.img`,
    arch: 'x86_64',
    variant: 'server',
    digest: 'sha256:8800651811af9a85465ad1d552add729947bb16488dddb4a9b5305a3d97332b2',
    kernel: {
      location: `${release}/unpacked/${image}-vmlinuz-generic`,
      arch: 'x86_64',
      digest: 'sha256:7efd88a7facf80874d781ccb2c2421f0aaaa575e3b94760430619e826f490117',
      cmdline: 'root=LABEL=cloudimg-rootfs ro console=tty1 console=ttyS0 no_timer_check',
    },
    initrd: {
      location: `${release}/unpacked/${image}-initrd-generic`,
      arch: 'x86_64',
      digest: 'sha256:c9f23b4ac34a20df6a1d344b44bf2597faa48799d814cbc206b3974c6ead5215',
    },
  }]
}

async function prepareContainerdArchive(goarch, directory, {
  platform = process.platform,
  runImpl = run,
  resolveShell = () => require('./installed-fixture.cjs').systemExecutable('bash', process.env),
} = {}) {
  const archive = selectArchive(JSON.parse(runImpl('limactl', ['info'], true)), goarch)
  const destination = path.resolve(directory)
  fs.mkdirSync(destination, { mode: 0o700 })
  const compressed = path.join(destination, 'nerdctl-full.tar.gz')
  const uncompressed = path.join(destination, 'nerdctl-full.tar')
  console.error('Preparing checksum-verified containerd files on the host for software-emulated Lima.')
  if (platform === 'win32') {
    const shell = await resolveShell()
    const gitRoot = path.win32.dirname(path.win32.dirname(shell))
    const env = {
      ...process.env,
      PATH: [path.win32.join(gitRoot, 'usr', 'bin'), process.env.PATH || process.env.Path || ''].join(';'),
    }
    runImpl(shell, ['--noprofile', '--norc', path.join(__dirname, 'download-file.sh'),
      archive.location, compressed, archive.digest.slice('sha256:'.length)], false, env)
  } else {
    runImpl('bash', [path.join(__dirname, 'download-file.sh'), archive.location, compressed, archive.digest.slice('sha256:'.length)])
  }
  const digest = await decompressArchive(compressed, uncompressed, archive.digest.slice('sha256:'.length))
  fs.unlinkSync(compressed)
  return `.containerd.archives = ${JSON.stringify([{ location: uncompressed, arch: archive.arch, digest }])}`
}

if (require.main === module) {
  const args = process.argv.slice(2)
  if (args.length === 1 && args[0] === '--dependencies') {
    process.stdout.write(`${dependencyProvisionOverride()}\n`)
  } else if (args.length === 1 && args[0] === '--intel-tcg-images') {
    process.stdout.write(`.images = ${JSON.stringify(intelTCGImages())}\n`)
  } else if (args.length !== 2) {
    console.error('usage: lima-containerd-archive.cjs <amd64|arm64> <new-directory> | --dependencies | --intel-tcg-images')
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

module.exports = { selectArchive, decompressArchive, dependencyProvisionOverride, intelTCGImages, prepareContainerdArchive }
