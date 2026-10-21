const assert = require('node:assert/strict')
const { execFile } = require('node:child_process')
const { createHash } = require('node:crypto')
const fs = require('node:fs')
const http = require('node:http')
const os = require('node:os')
const path = require('node:path')
const test = require('node:test')
const { promisify } = require('node:util')

const execFileAsync = promisify(execFile)
const downloadScript = path.join(__dirname, 'download-file.sh')

function temporaryDirectory(t) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'porto-download-'))
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }))
  return directory
}

async function serveFixture(t, handler) {
  const server = http.createServer(handler)
  t.after(() => new Promise((resolve) => server.close(resolve)))
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })
  const address = server.address()
  assert(address && typeof address === 'object')
  return `http://127.0.0.1:${address.port}`
}

test('uses an extended retry window and redacts URL query strings from logs', async (t) => {
  const temporary = temporaryDirectory(t)
  const bin = path.join(temporary, 'bin')
  const curlArguments = path.join(temporary, 'curl-arguments')
  const output = path.join(temporary, 'asset')
  fs.mkdirSync(bin)
  fs.writeFileSync(
    path.join(bin, 'curl'),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$CURL_ARGUMENTS_FILE"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output" ]; then
    printf 'downloaded' > "$2"
    exit 0
  fi
  shift
done
exit 2
`,
  )
  fs.chmodSync(path.join(bin, 'curl'), 0o755)

  const url = 'https://downloads.example.test/runtime.tar.gz?token=secret'
  const { stderr } = await execFileAsync('bash', [downloadScript, url, output], {
    env: {
      ...process.env,
      CURL_ARGUMENTS_FILE: curlArguments,
      PATH: `${bin}${path.delimiter}${process.env.PATH}`,
    },
  })

  assert.equal(fs.readFileSync(output, 'utf8'), 'downloaded')
  assert.deepEqual(fs.readFileSync(curlArguments, 'utf8').trim().split('\n'), [
    '--fail',
    '--location',
    '--retry',
    '10',
    '--retry-all-errors',
    '--retry-max-time',
    '300',
    '--connect-timeout',
    '30',
    '--silent',
    '--show-error',
    url,
    '--output',
    output,
  ])
  assert.match(stderr, /Downloading https:\/\/downloads\.example\.test\/runtime\.tar\.gz/)
  assert.doesNotMatch(stderr, /token=secret/)
})

test('retries a transient server failure', async (t) => {
  const temporary = temporaryDirectory(t)
  const output = path.join(temporary, 'asset')
  let requests = 0
  const server = http.createServer((_request, response) => {
    requests += 1
    if (requests === 1) {
      response.writeHead(500, { 'Retry-After': '0' })
      response.end('temporary failure')
      return
    }
    response.end('runtime payload')
  })
  t.after(() => new Promise((resolve) => server.close(resolve)))
  await new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  })

  const address = server.address()
  assert(address && typeof address === 'object')
  await execFileAsync(
    'bash',
    [downloadScript, `http://127.0.0.1:${address.port}/runtime.tar.gz`, output],
    {
      env: {
        ...process.env,
        PORTO_DOWNLOAD_RETRIES: '2',
        PORTO_DOWNLOAD_RETRY_MAX_TIME: '10',
      },
    },
  )

  assert.equal(requests, 2)
  assert.equal(fs.readFileSync(output, 'utf8'), 'runtime payload')
})

test('reuses only checksum-verified bytes, even when the download URL changes', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  const payload = 'synthetic verified runtime'
  const checksum = createHash('sha256').update(payload).digest('hex')
  let requests = 0
  const url = await serveFixture(t, (_request, response) => {
    requests += 1
    response.end(payload)
  })
  const options = { env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache } }
  const first = path.join(temporary, 'first')
  const second = path.join(temporary, 'second')

  await execFileAsync('bash', [downloadScript, `${url}/first`, first, checksum], options)
  const { stderr } = await execFileAsync(
    'bash', [downloadScript, `${url}/second?token=secret`, second, checksum.toUpperCase()], options,
  )

  assert.equal(requests, 1)
  assert.equal(fs.readFileSync(first, 'utf8'), payload)
  assert.equal(fs.readFileSync(second, 'utf8'), payload)
  assert.deepEqual(fs.readdirSync(cache), [checksum])
  assert.match(stderr, /checksum-verified download cache/)
  assert.doesNotMatch(stderr, /token=secret/)
})

test('rejects corrupt cached bytes instead of accepting them or hiding the failure', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  const output = path.join(temporary, 'output')
  const checksum = createHash('sha256').update('expected runtime').digest('hex')
  fs.mkdirSync(cache)
  fs.writeFileSync(path.join(cache, checksum), 'corrupt cache')
  let requests = 0
  const url = await serveFixture(t, (_request, response) => {
    requests += 1
    response.end('expected runtime')
  })

  await assert.rejects(
    execFileAsync('bash', [downloadScript, url, output, checksum], {
      env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache },
    }),
    /checksum mismatch/i,
  )
  assert.equal(requests, 0)
  assert.equal(fs.existsSync(output), false)
  assert.equal(fs.readFileSync(path.join(cache, checksum), 'utf8'), 'corrupt cache')
})

test('does not publish or cache a download with the wrong checksum', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  const output = path.join(temporary, 'output')
  const checksum = createHash('sha256').update('expected runtime').digest('hex')
  const url = await serveFixture(t, (_request, response) => response.end('wrong runtime'))

  await assert.rejects(
    execFileAsync('bash', [downloadScript, url, output, checksum], {
      env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache },
    }),
    /checksum mismatch/i,
  )
  assert.deepEqual(fs.readdirSync(cache), [])
  assert.equal(fs.existsSync(output), false)
})

test('removes partial cache entries after a failed download', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  const output = path.join(temporary, 'output')
  const checksum = createHash('sha256').update('expected runtime').digest('hex')
  const url = await serveFixture(t, (_request, response) => {
    response.writeHead(500)
    response.end('failure')
  })

  await assert.rejects(execFileAsync('bash', [downloadScript, url, output, checksum], {
    env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache, PORTO_DOWNLOAD_RETRIES: '0' },
  }))
  assert.deepEqual(fs.readdirSync(cache), [])
  assert.equal(fs.existsSync(output), false)
})

test('does not cache downloads without an expected checksum', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  let requests = 0
  const url = await serveFixture(t, (_request, response) => {
    requests += 1
    response.end('uncached manifest')
  })
  const options = { env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache } }

  await execFileAsync('bash', [downloadScript, url, path.join(temporary, 'first')], options)
  await execFileAsync('bash', [downloadScript, url, path.join(temporary, 'second')], options)

  assert.equal(requests, 2)
  assert.equal(fs.existsSync(cache), false)
})

test('publishes complete cache entries when downloads run concurrently', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  const payload = 'synthetic concurrent runtime'
  const checksum = createHash('sha256').update(payload).digest('hex')
  const url = await serveFixture(t, (_request, response) => response.end(payload))
  const outputs = Array.from({ length: 4 }, (_, index) => path.join(temporary, `output-${index}`))

  await Promise.all(outputs.map((output) => execFileAsync('bash', [downloadScript, url, output, checksum], {
    env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache },
  })))

  assert.deepEqual(fs.readdirSync(cache), [checksum])
  assert.equal(fs.readFileSync(path.join(cache, checksum), 'utf8'), payload)
  for (const output of outputs) assert.equal(fs.readFileSync(output, 'utf8'), payload)
})

test('rejects an invalid cache checksum before making a request', async (t) => {
  const temporary = temporaryDirectory(t)
  const cache = path.join(temporary, 'cache')
  let requests = 0
  const url = await serveFixture(t, (_request, response) => {
    requests += 1
    response.end('unused')
  })

  await assert.rejects(
    execFileAsync('bash', [downloadScript, url, path.join(temporary, 'output'), '../invalid'], {
      env: { ...process.env, PORTO_DOWNLOAD_CACHE: cache },
    }),
    /expected SHA-256 checksum/i,
  )
  assert.equal(requests, 0)
  assert.equal(fs.existsSync(cache), false)
})
