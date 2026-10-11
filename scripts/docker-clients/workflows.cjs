const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const fs = require('node:fs/promises')
const path = require('node:path')
const { GenericContainer, Wait } = require('testcontainers')

function run(command, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { env: process.env, stdio: ['ignore', 'pipe', 'pipe'] })
    let stdout = ''
    let stderr = ''
    child.stdout.setEncoding('utf8')
    child.stderr.setEncoding('utf8')
    child.stdout.on('data', (data) => { stdout += data })
    child.stderr.on('data', (data) => { stderr += data })
    child.once('error', reject)
    child.once('exit', (code, signal) => {
      if (code !== 0) reject(new Error(`${command} ${args.join(' ')} exited ${code ?? signal}\n${stdout}\n${stderr}`))
      else resolve(stdout)
    })
  })
}

async function main() {
  const image = process.env.PORTO_CLIENT_TEST_IMAGE
  const workspace = process.env.PORTO_CLIENT_TEST_WORKSPACE
  const suite = process.env.PORTO_CLIENT_TEST_SUITE
  assert.ok(image && workspace && suite && process.env.DOCKER_HOST, 'Run through the isolated Go integration harness')
  assert.equal(process.env.TESTCONTAINERS_RYUK_DISABLED, 'true', 'The harness owns cleanup; no host socket is mounted into the guest')
  const dockerHost = process.env.DOCKER_HOST
  if (process.platform === 'win32') {
    process.env.DOCKER_HOST = dockerHost.replace('npipe:////./pipe/', 'npipe:////.\\pipe\\')
  }

  const container = await new GenericContainer(image)
    .withName(`${suite}-testcontainers`)
    .withLabels({ 'porto.test.suite': suite })
    .withNetworkMode('none')
    .withCommand(['sh', '-c', 'printf "porto-testcontainers-ready\\n"; sleep 300'])
    .withWaitStrategy(Wait.forLogMessage('porto-testcontainers-ready'))
    .withStartupTimeout(120_000)
    .start()
  try {
    const result = await container.exec(['sh', '-c', 'cat /fixture'])
    assert.equal(result.exitCode, 0)
    assert.equal(result.stdout.trim(), 'porto-synthetic-client-fixture')
  } finally {
    await container.stop({ remove: true, removeVolumes: true })
    process.env.DOCKER_HOST = dockerHost
  }
  console.log('Testcontainers create/start/log-wait/exec/cleanup passed')

  const configDirectory = path.join(workspace, '.devcontainer')
  await fs.mkdir(configDirectory, { recursive: true })
  const config = path.join(configDirectory, 'devcontainer.json')
  const workspaceVolume = `${suite}-workspace`
  await run('docker', ['volume', 'create', '--label', `porto.test.suite=${suite}`, workspaceVolume])
  await fs.writeFile(config, JSON.stringify({
    name: 'Porto synthetic development container',
    image,
    remoteUser: 'root',
    userEnvProbe: 'none',
    workspaceFolder: '/workspaces/porto-synthetic',
    workspaceMount: `type=volume,source=${workspaceVolume},target=/workspaces/porto-synthetic`,
    runArgs: ['--network', 'none'],
    postCreateCommand: ['sh', '-c', 'cat /fixture > /workspaces/porto-synthetic/client-fixture'],
  }))
  const cli = path.join(path.dirname(require.resolve('@devcontainers/cli/package.json')), 'devcontainer.js')
  let developmentContainer
  try {
    const output = await run(process.execPath, [
      cli, 'up', '--workspace-folder', workspace, '--config', config,
      '--id-label', `porto.test.suite=${suite}`,
      '--log-format', 'json', '--log-level', 'info',
    ])
    const results = output.trim().split('\n').filter(Boolean).map((line) => JSON.parse(line))
    const result = results.find((entry) => entry.outcome === 'success')
    assert.ok(result?.containerId, `Dev Containers did not report successful creation: ${output}`)
    developmentContainer = result.containerId
    const executed = await run(process.execPath, [
      cli, 'exec', '--workspace-folder', workspace, '--config', config,
      '--id-label', `porto.test.suite=${suite}`,
      'sh', '-c', 'cat /workspaces/porto-synthetic/client-fixture',
    ])
    assert.equal(executed.trim(), 'porto-synthetic-client-fixture')
  } finally {
    if (developmentContainer) await run('docker', ['rm', '--force', '--volumes', developmentContainer])
    await run('docker', ['volume', 'rm', workspaceVolume])
  }
  console.log('Dev Containers up/post-create/exec/cleanup passed')
}

main().catch((error) => {
  console.error(error)
  process.exitCode = 1
})
