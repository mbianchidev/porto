const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const path = require('node:path')

const {
  environment, fileDigest, prepare, run, save, target, within, workspacePath,
} = require('./installed-fixture.cjs')
const { acceptance, preflight, removed } = require('./installed-acceptance.cjs')

async function releaseServer(artifact) {
  const asset = path.basename(artifact)
  const checksum = `${await fileDigest(artifact)}  ${asset}\n`
  const server = http.createServer((request, response) => {
    if (request.method !== 'GET') {
      response.writeHead(405).end()
    } else if (request.url === '/SHA256SUMS') {
      response.writeHead(200, { 'Content-Type': 'text/plain' }).end(checksum)
    } else if (request.url === `/${encodeURI(asset)}` || request.url === `/${asset}`) {
      response.writeHead(200, { 'Content-Length': fs.statSync(artifact).size })
      const stream = fs.createReadStream(artifact)
      stream.on('error', () => response.destroy())
      stream.pipe(response)
    } else response.writeHead(404).end()
  })
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  return {
    url: `http://127.0.0.1:${server.address().port}`,
    close: async () => await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve())),
  }
}

function uninstall(fixture) {
  assert.ok(within(fixture.directory, fixture.installRoot), 'Only remove the synthetic installed application')
  const bin = path.join(fixture.home, '.local', 'bin')
  for (const name of ['porto', 'porto-desktop', 'docker', 'dive']) {
    const entry = path.join(bin, name)
    if (!fs.existsSync(entry)) continue
    if (!fs.lstatSync(entry).isSymbolicLink()) continue
    if (within(fixture.installRoot, path.resolve(path.dirname(entry), fs.readlinkSync(entry)))) fs.unlinkSync(entry)
  }
  const desktopEntry = path.join(fixture.home, '.local', 'share', 'applications', 'porto.desktop')
  if (fs.existsSync(desktopEntry)) {
    assert.ok(fs.readFileSync(desktopEntry, 'utf8').split(/\r?\n/).includes(`Exec="${fixture.installRoot}/Porto"`),
      'Refusing to remove a desktop entry not owned by the synthetic installation')
    fs.unlinkSync(desktopEntry)
  }
  fs.rmSync(fixture.installRoot, { recursive: true })
}

async function main(args) {
  assert.equal(process.env.GITHUB_ACTIONS, 'true', 'Installed package acceptance is restricted to disposable CI hosts')
  const [artifactPath, version, expectedTarget, directory, backend = 'qemu-tcg'] = args
  assert.ok(artifactPath && version && expectedTarget && directory,
    'Usage: installed-package.cjs <built-dmg-or-linux-archive> <version> <goos/goarch> <new-directory> [default|qemu-tcg]')
  assert.equal(target(), expectedTarget, 'Native installed execution requires the matching runner')
  assert.notEqual(process.platform, 'win32', 'Windows CI uses the native NSIS update/uninstall fixture and the same acceptance harness')
  assert.match(version, /^[0-9A-Za-z.+-]+$/)
  assert.ok(['default', 'qemu-tcg'].includes(backend))
  const artifact = workspacePath(artifactPath)
  const [goos, arch] = expectedTarget.split('/')
  assert.equal(path.basename(artifact), `porto-desktop_${version}_${goos}_${arch}.${goos === 'darwin' ? 'dmg' : 'tar.gz'}`)
  const fixture = prepare(directory, expectedTarget)
  fixture.backend = backend
  save(fixture)
  const env = environment(fixture)
  await preflight(fixture, env)
  const release = await releaseServer(artifact)
  try {
    const install = async () => {
      await preflight(fixture, env)
      const result = await run('/bin/sh', [path.join(__dirname, 'install-desktop.sh')], {
        env: {
          ...env,
          PORTO_VERSION: `v${version}`, PORTO_RELEASE_BASE_URL: release.url,
          PORTO_INSTALL_DIR: process.platform === 'darwin' ? path.dirname(fixture.installRoot) : fixture.installRoot,
          PORTO_BIN_DIR: path.join(fixture.home, '.local', 'bin'),
          PORTO_NO_LAUNCH: '1',
        },
        cwd: fixture.home, timeout: 10 * 60 * 1000,
      })
      process.stdout.write(result.stdout)
      process.stderr.write(result.stderr)
    }
    await install()
    await acceptance(fixture)
    await install()
    await acceptance(fixture, true)
    await preflight(fixture, env)
    // macOS/Linux ship an application, not a native uninstaller. Exercise the
    // documented owned-app/owned-entry removal, never delete runtime data.
    uninstall(fixture)
    await removed(fixture)
  } finally {
    await release.close()
  }
}

if (require.main === module) {
  main(process.argv.slice(2)).catch((error) => {
    console.error(error)
    process.exitCode = 1
  })
}

module.exports = { main, releaseServer, uninstall }
