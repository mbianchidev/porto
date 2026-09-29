const assert = require('node:assert/strict')
const { execFile } = require('node:child_process')
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
