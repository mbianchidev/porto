import { expect, test, type Page } from '@playwright/test'
import { isLogRecord, LogBuffer, type LogRecord } from '../src/containerLogs'
import type { FileEntry, FileListing } from '../src/dataTypes'

const fixture = {
  id: 'synthetic-container-1', name: 'fixture-api', image: 'fixture.invalid/app:fixture',
  state: 'running', status: 'Running', ports: '', networks: '', mounts: '',
  createdAt: '2026-01-01T00:00:00Z', taskPresent: true, pid: 10,
  oomKilled: false, restartCount: 0, health: { status: 'disabled' }, resources: {},
  composeProject: 'fixture-stack', composeService: 'api',
}
const snapshot = {
  instanceId: 'fixture', revision: 1, available: true, stale: false, containers: [fixture],
  capabilities: Object.fromEntries([
    'directInventory', 'lifecycleEvents', 'directCreation', 'taskRecreation',
    'execLifecycle', 'healthUpdates', 'networkUpdates', 'checkpointRestore',
  ].map((name) => [name, { supported: true }])),
}

declare global {
  interface Window {
    fixtureLogSources?: Array<{ target: EventTarget; closed: boolean }>
    fixtureContainerSource?: EventTarget
    fixtureRequests?: Array<{ path: string; method: string; aborted: boolean }>
    fixtureHeldPath?: string
    fixtureClipboard?: string
  }
}

const containerPrefix = '/api/docker/containers/synthetic-container-1'

async function mockLogSources(page: Page) {
  await page.addInitScript(() => {
    const Original = window.EventSource
    Object.defineProperty(window, 'EventSource', { value: function (url: string) {
      if (!String(url).includes('/logs/stream')) return new Original(url)
      const target = Object.assign(new EventTarget(), {
        url, readyState: 1,
        onopen: null as ((event: Event) => void) | null,
        onerror: null as ((event: Event) => void) | null,
        close() { source.closed = true },
      })
      const source = { target, closed: false }
      window.fixtureLogSources ??= []
      window.fixtureLogSources.push(source)
      target.addEventListener('error', (event) => target.onerror?.(event))
      queueMicrotask(() => target.onopen?.(new Event('open')))
      return target
    } })
  })
}

async function emitLogs(page: Page, records: LogRecord[]) {
  await page.evaluate((records) => {
    const source = window.fixtureLogSources?.find((item) => !item.closed)
    if (!source) throw new Error('Fixture log stream missing')
    for (const record of records) source.target.dispatchEvent(new MessageEvent('log', { data: JSON.stringify(record) }))
  }, records)
}

async function openInspector(page: Page, tab: string) {
  await page.goto('/#/containers')
  await page.getByText('fixture-api', { exact: true }).first().click()
  await page.getByRole('tab', { name: tab, exact: true }).click()
}

async function probeRequests(page: Page) {
  await page.addInitScript(() => {
    const original = window.fetch.bind(window)
    window.fixtureRequests = []
    window.fetch = (input, init) => {
      const path = new URL(input instanceof Request ? input.url : String(input), window.location.href).pathname
      if (!path.startsWith('/api/docker/containers/synthetic-container-1')) return original(input, init)
      const request = { path, method: init?.method ?? 'GET', aborted: false }
      window.fixtureRequests?.push(request)
      const signal = init?.signal ?? (input instanceof Request ? input.signal : undefined)
      signal?.addEventListener('abort', () => { request.aborted = true }, { once: true })
      if (path === window.fixtureHeldPath) {
        return new Promise<Response>((_resolve, reject) => {
          signal?.addEventListener('abort', () => reject(new DOMException('Synthetic cancellation', 'AbortError')), { once: true })
        })
      }
      return original(input, init)
    }
  })
}

async function mockFiles(page: Page, readOnly = false) {
  const requests: Array<{ path: string; method: string; identity: string; sha256: string; confirm: string; body: string | null }> = []
  const entry = (path: string, type = 'file', size = 9): FileEntry => ({
    path, type, size, mode: 0o644, uid: 0, gid: 0, modifiedAt: '2026-01-01T00:00:00Z',
  })
  const contents = new Map([['fixture.txt', 'synthetic'], ['second.txt', 'second fixture'], ['mounted/config.txt', 'mounted fixture']])
  await page.route(`**${containerPrefix}/file*`, async (route) => {
    const url = new URL(route.request().url())
    const path = url.searchParams.get('path') ?? '.'
    const method = route.request().method()
    const json = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) })
    requests.push({
      path, method, identity: url.searchParams.get('identity') ?? '', sha256: url.searchParams.get('sha256') ?? '',
      confirm: url.searchParams.get('confirm') ?? '', body: route.request().postData(),
    })
    if (path.includes('..') || path.startsWith('/')) return json({ message: 'Invalid path: stay inside the selected resource.' }, 400)
    if (url.pathname.endsWith('/files')) {
      const listing: FileListing = {
        resource: { kind: 'container', name: 'fixture-api', id: fixture.id, readOnly },
        identity: 'synthetic-fingerprint', path,
        readOnly: readOnly || path === 'mounted', truncated: false,
        entries: path === '.' ? [
          ...[...contents.keys()].filter((name) => !name.includes('/')).map((name) => entry(name)),
          entry('binary.bin'), entry('large.txt', 'file', 300 * 1024), entry('mounted', 'directory'),
          { ...entry('link', 'symlink'), linkTarget: '/outside' },
        ] : [entry('mounted/config.txt')],
        mounts: [{ path: '/mounted', kind: 'volume', source: 'fixture-volume', readOnly: true }],
        message: 'Synthetic shell-independent container snapshot access.',
      }
      return json(listing)
    }
    if (url.pathname.endsWith('/download')) {
      return route.fulfill({
        contentType: 'application/octet-stream', headers: { 'Content-Disposition': 'attachment' },
        body: path === 'binary.bin' ? Buffer.from('synthetic\0binary') : Buffer.from('x'.repeat(300 * 1024)),
      })
    }
    if (method === 'PUT') {
      if (readOnly || path.startsWith('mounted/')) return json({ message: 'This filesystem or mount is read-only.' }, 501)
      if (contents.has(path) && !url.searchParams.get('sha256')) return json({ message: 'Overwrite requires a current checksum.' }, 409)
      contents.set(path, route.request().postData() ?? '')
      return json({ written: true })
    }
    if (method === 'DELETE') {
      contents.delete(path)
      return json({ deleted: true })
    }
    if (!contents.has(path)) return json({ message: 'Text preview unavailable; use Download instead.' }, 501)
    return json({ path, text: contents.get(path), sha256: 'synthetic-checksum', readOnly: readOnly || path.startsWith('mounted/') })
  })
  return requests
}

async function mockRuntime(page: Page, runtimeSnapshot = snapshot) {
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const values: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false },
      '/api/docker/status': { available: true, enabled: true, context: 'porto' },
      '/api/docker/cleanup': { runs: [], enabled: false, dockerEnabled: true },
      '/api/files/capabilities': { supported: false, driver: 'fixture', message: 'Synthetic missing driver', fallback: 'Use Files or archives.' },
      '/api/files/attachments': [],
      '/api/data/operations': [],
      '/api/docker/backups': [],
      '/api/docker/containers/snapshot': runtimeSnapshot,
      '/api/docker/containers': runtimeSnapshot.containers,
      '/api/docker/images': [],
      '/api/docker/containers/synthetic-container-1': {
        Id: fixture.id, Config: { Env: ['FIXTURE_TOKEN=synthetic-secret'], Labels: {} },
        HostConfig: { RestartPolicy: { Name: 'no' } }, Mounts: [], NetworkSettings: {},
      },
      '/api/docker/containers/synthetic-container-1/stats': {
        available: true, history: [{ read: '2026-01-01T00:00:02Z', cpuMillicores: 500, memoryBytes: 1024, pids: 2 }],
        current: { read: '2026-01-01T00:00:02Z', cpuMillicores: 500, memoryBytes: 1024, pids: 2 },
      },
    }
    if (path.endsWith('/containers/events')) {
      await route.fulfill({ contentType: 'text/event-stream', body: `event: snapshot\ndata: ${JSON.stringify(runtimeSnapshot)}\n\n` })
      return
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
}

test('Compose status and running counts follow live task snapshots', async ({ page }, testInfo) => {
  const services = ['api', 'web', 'database'].map((service, index) => ({
    ...fixture, id: `synthetic-container-${index + 1}`, name: `fixture-${service}`,
    composeService: service, state: 'restarting', status: 'Restarting', taskPresent: false, pid: 0,
  }))
  const oldContainer = {
    ...fixture, id: 'synthetic-container-4', name: 'fixture-api-old',
    state: 'created', status: 'Created', taskPresent: false, pid: 0,
  }
  await mockRuntime(page, { ...snapshot, containers: [...services, oldContainer] })
  await page.addInitScript(() => {
    const Original = window.EventSource
    Object.defineProperty(window, 'EventSource', { value: function (url: string) {
      if (!String(url).includes('/containers/events')) return new Original(url)
      const target = new EventTarget()
      window.fixtureContainerSource = target
      const source: EventTarget & { onopen: ((event: Event) => void) | null } = Object.assign(target, {
        url, readyState: 1, onopen: null, onerror: null,
        close() {
          if (window.fixtureContainerSource === target) window.fixtureContainerSource = undefined
        },
      })
      queueMicrotask(() => source.onopen?.(new Event('open')))
      return source
    } })
  })
  await page.goto('/#/containers', { waitUntil: 'networkidle' })
  const group = page.locator('.containerComposeGroup')
  const summary = group.locator('summary')
  await expect(summary).toContainText('3 service(s) · 4 container(s) · 0 running')

  async function publishSnapshot(nextSnapshot: typeof snapshot) {
    await page.evaluate((nextSnapshot) => {
      const source = window.fixtureContainerSource
      if (!source) throw new Error('Fixture container stream missing')
      source.dispatchEvent(new MessageEvent('snapshot', { data: JSON.stringify(nextSnapshot) }))
    }, nextSnapshot)
  }

  const runningSnapshot = {
    ...snapshot, revision: 2,
    containers: [
      ...services.map((container, index) => ({
        ...container, state: 'running', status: 'Up', taskPresent: true, pid: 42 + index,
      })),
      { ...oldContainer, state: 'exited', status: 'Exited (0)', taskPresent: true, pid: 50, exitCode: 0 },
    ],
  }
  await publishSnapshot(runningSnapshot)
  await expect(summary).toContainText('3 service(s) · 4 container(s) · 3 running')
  await expect(summary).toContainText('running application state')
  await expect(group.getByRole('option', { name: /Status: Up(?:\s|$)/ })).toHaveCount(3)
  await expect(group.getByRole('option', { name: /Status: Exited \(0\)/ })).toBeVisible()
  await expect(group.getByRole('option', { name: /Status: Restarting/ })).toHaveCount(0)
  await expect(group.getByRole('button', { name: 'Remove container (stop it first)', exact: true, disabled: true })).toHaveCount(3)
  await page.screenshot({ path: testInfo.outputPath('compose-running.png'), fullPage: true })

  await publishSnapshot({
    ...runningSnapshot, revision: 3,
    containers: runningSnapshot.containers.map((container, index) => index === 0
      ? { ...container, state: 'exited', status: 'Exited (1)', exitCode: 1 }
      : container),
  })
  await expect(summary).toContainText('3 service(s) · 4 container(s) · 2 running')
  await expect(group.getByRole('option', { name: /Status: Up(?:\s|$)/ })).toHaveCount(2)
  await expect(group.getByRole('option', { name: /Status: Exited \(1\)/ })).toBeVisible()
})

for (const grouped of [true, false]) {
  test(`inactive ${grouped ? 'Compose' : 'standalone'} containers remain startable with restart policies`, async ({ page }, testInfo) => {
    const containers = [
      {
        ...fixture, id: 'synthetic-idle', name: 'fixture-idle', state: 'created', status: 'Created',
        taskPresent: false, pid: 0, restartPolicy: 'unless-stopped',
        composeProject: grouped ? 'fixture-idle-stack' : '', composeService: grouped ? 'api' : '',
      },
      {
        ...fixture, id: 'synthetic-exited', name: 'fixture-exited', state: 'exited', status: 'Exited (1)',
        exitCode: 1, restartPolicy: 'always',
        composeProject: grouped ? 'fixture-idle-stack' : '', composeService: grouped ? 'worker' : '',
      },
    ]
    await mockRuntime(page, { ...snapshot, containers })
    await page.goto('/#/containers', { waitUntil: 'networkidle' })
    for (const container of containers) {
      const row = page.locator('.inventoryRow').filter({
        has: page.getByRole('option', { name: new RegExp(container.name) }),
      })
      await expect(row.getByRole('option')).toContainText(`Status: ${container.status}`)
      await expect(row.getByRole('button', { name: 'Start container', exact: true })).toBeEnabled()
      await expect(row.getByRole('button', { name: 'Stop container', exact: true })).toBeDisabled()
      await expect(row.getByRole('button', { name: 'Restart container', exact: true })).toBeDisabled()
      await expect(row.getByRole('button', { name: 'Remove container', exact: true })).toBeEnabled()
    }
    await expect(page.getByRole('option', { name: /Status: Restarting/ })).toHaveCount(0)
    if (grouped) {
      await expect(page.locator('.containerComposeGroup summary')).toContainText('2 service(s) · 2 container(s) · 0 running')
      await page.screenshot({ path: testInfo.outputPath('inactive-containers.png'), fullPage: true })
    }
  })
}

test('logs remain bounded, searchable, stream-filtered and close on tab switch', async ({ page }) => {
      await mockRuntime(page)
      await mockLogSources(page)
      await page.goto('/#/containers')
      await page.getByText('fixture-api', { exact: true }).first().click()
      await page.getByRole('tab', { name: 'Logs', exact: true }).click()
      await expect(page.getByText('Live', { exact: true })).toBeVisible()
      await page.evaluate(() => {
        const source = window.fixtureLogSources?.find((item) => !item.closed)
        if (!source) throw new Error('Fixture log stream missing')
        for (let index = 0; index < 6000; index++) {
          source.target.dispatchEvent(new MessageEvent('log', { data: JSON.stringify({
            timestamp: '2026-01-01T00:00:00Z', stream: index % 2 ? 'stderr' : 'stdout', text: `synthetic-line-${index}\n`,
          }) }))
        }
      })
      const output = page.getByRole('log', { name: 'Container output' })
      await expect(output.getByText('synthetic-line-5999', { exact: true })).toBeVisible()
      await expect(output.getByText('synthetic-line-0', { exact: true })).toHaveCount(0)
      await expect(page.getByText(/1000 older record/)).toBeVisible()
      await page.getByLabel('Search logs').fill('5999')
      await expect(output.getByText('synthetic-line-5999', { exact: true })).toBeVisible()
      await page.getByLabel('Log stream').selectOption('stdout')
      await expect(output.getByText('synthetic-line-5999', { exact: true })).toHaveCount(0)
      await page.getByRole('tab', { name: 'Overview', exact: true }).click()
      await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.every((source) => source.closed))).toBe(true)
    })

    test('metadata hides sensitive values until explicit consent', async ({ page }) => {
      await mockRuntime(page)
      await page.goto('/#/containers')
      await page.getByText('fixture-api', { exact: true }).first().click()
      await page.getByRole('tab', { name: 'Inspect', exact: true }).click()
      await page.getByText('Environment', { exact: true }).click()
      await expect(page.getByText(/FIXTURE_TOKEN=\[redacted\]/).first()).toBeVisible()
      await expect(page.getByText(/synthetic-secret/)).toHaveCount(0)
      page.once('dialog', (dialog) => dialog.accept())
      await page.getByRole('button', { name: 'Reveal sensitive values' }).click()
      await expect(page.getByText(/synthetic-secret/).first()).toBeVisible()
    })

test('container inspector exposes the complete phase-two tabs', async ({ page }) => {
  await mockRuntime(page)
  await page.goto('/#/containers')
  await page.getByText('fixture-api', { exact: true }).first().click()
  for (const name of ['Overview', 'Logs', 'Files', 'Stats', 'Inspect', 'Terminal']) {
    await expect(page.getByRole('tab', { name, exact: true })).toBeVisible()
  }
  await page.getByRole('tab', { name: 'Stats', exact: true }).click()
  await expect(page.getByText('500m', { exact: true })).toBeVisible()
  await expect(page.getByText(/Unavailable/).first()).toBeVisible()
})

test('a failed lazy inspector view leaves the inventory and other tabs usable', async ({ page }) => {
  await mockRuntime(page)
  await page.route('**/src/components/ContainerDataInspector.tsx*', (route) => route.abort('connectionfailed'))
  await openInspector(page, 'Stats')
  await expect(page.getByRole('alert').filter({ hasText: 'Inspector view unavailable' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Reload Porto', exact: true })).toBeVisible()
  await page.getByRole('tab', { name: 'Overview', exact: true }).click()
  await expect(page.getByText('Runtime detail', { exact: true })).toBeVisible()
  await expect(page.getByRole('alert').filter({ hasText: 'Inspector view unavailable' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Close inspector' }).click()
  await expect(page.locator('.containerComposeGroup')).toBeVisible()
})

test('log buffer enforces its byte limit and rejects malformed record metadata', () => {
  const buffer = new LogBuffer()
  for (let index = 0; index < 140; index++) buffer.push({ stream: 'stdout', text: `${index}:${'x'.repeat(32700)}` })
  expect(buffer.snapshot().length).toBeLessThanOrEqual(64)
  expect(buffer.dropped).toBeGreaterThan(0)
  expect(isLogRecord({ stream: 'stdout', text: 'synthetic', timestamp: 42 })).toBe(false)
  expect(isLogRecord({ stream: 'stderr', text: 'synthetic', timestamp: 'invalid' })).toBe(false)
  expect(isLogRecord({ stream: 'stdout', text: 'synthetic', partial: 'invalid' })).toBe(false)
  buffer.clear()
  expect(buffer.snapshot()).toEqual([])
  expect(buffer.dropped).toBe(0)
  buffer.push({ stream: 'stderr', text: 'synthetic after clear' })
  expect(buffer.snapshot()).toHaveLength(1)
})

test('clearing logs keeps the subscription and accepts only new output', async ({ page }) => {
  await mockRuntime(page)
  await mockLogSources(page)
  await openInspector(page, 'Logs')
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.length ?? 0)).toBeGreaterThan(0)
  const count = await page.evaluate(() => window.fixtureLogSources?.length)
  await emitLogs(page, [{ stream: 'stdout', text: 'synthetic-before-clear\n' }])
  const output = page.getByRole('log', { name: 'Container output' })
  await expect(output).toContainText('synthetic-before-clear')
  await page.getByRole('button', { name: 'Clear buffer' }).click()
  await expect(output).not.toContainText('synthetic-before-clear')
  expect(await page.evaluate(() => window.fixtureLogSources?.length)).toBe(count)
  await emitLogs(page, [{ stream: 'stderr', text: 'synthetic-after-clear\n' }])
  await expect(output).toContainText('synthetic-after-clear')
  await page.getByRole('button', { name: 'Close inspector' }).click()
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.every((source) => source.closed))).toBe(true)
})

test('log connection loss is actionable and reconnect does not retain stale messages', async ({ page }) => {
  await mockRuntime(page)
  await mockLogSources(page)
  await openInspector(page, 'Logs')
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.some((source) => !source.closed))).toBe(true)
  await page.evaluate(() => window.fixtureLogSources?.find((source) => !source.closed)?.target.dispatchEvent(new Event('error')))
  await expect(page.getByText('Disconnected', { exact: true })).toBeVisible()
  await expect(page.getByText('Log connection lost. Reconnect to reload retained history.')).toBeVisible()
  await page.getByRole('button', { name: 'Reconnect logs' }).click()
  await expect(page.getByText('Live', { exact: true }).last()).toBeVisible()
  await expect(page.getByText('Log connection lost. Reconnect to reload retained history.')).toHaveCount(0)
})

test('paused log copy respects filtering and sanitization without inventing timestamps', async ({ page }) => {
  await mockRuntime(page)
  await mockLogSources(page)
  await page.addInitScript(() => Object.defineProperty(navigator, 'clipboard', {
    value: { writeText: async (text: string) => { window.fixtureClipboard = text } },
  }))
  await openInspector(page, 'Logs')
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.some((source) => !source.closed))).toBe(true)
  await emitLogs(page, [
    { stream: 'stdout', timestamp: '2026-01-01T00:00:00Z', text: '\u001b[31msynthetic stdout\u001b[0m\n' },
    { stream: 'stderr', text: 'synthetic stderr' },
  ])
  await page.getByRole('button', { name: 'Pause logs' }).click()
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.every((source) => source.closed))).toBe(true)
  await page.getByLabel('Log stream').selectOption('stderr')
  await page.getByRole('button', { name: 'Copy logs' }).click()
  await expect.poll(() => page.evaluate(() => window.fixtureClipboard)).toBe('untimed stderr synthetic stderr\n')
  await page.getByLabel('Log stream').selectOption('stdout')
  await page.getByRole('button', { name: 'Copy logs' }).click()
  await expect.poll(() => page.evaluate(() => window.fixtureClipboard)).toBe('2026-01-01T00:00:00Z stdout synthetic stdout\n')
  await page.getByRole('button', { name: 'Resume logs' }).click()
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.some((source) => !source.closed))).toBe(true)
})

test('invalid log capabilities disconnect without accepting queued output', async ({ page }) => {
  await mockRuntime(page)
  await mockLogSources(page)
  await openInspector(page, 'Logs')
  await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.some((source) => !source.closed))).toBe(true)
  await page.evaluate(() => {
    const source = window.fixtureLogSources?.find((source) => !source.closed)
    if (!source) throw new Error('Fixture log source missing')
    source.target.dispatchEvent(new MessageEvent('capabilities', { data: '{"streams":"invalid"}' }))
    source.target.dispatchEvent(new MessageEvent('log', { data: '{"stream":"stdout","text":"synthetic late record"}' }))
    source.target.dispatchEvent(new Event('error'))
  })
  await expect(page.getByText('Invalid log capability response', { exact: true })).toBeVisible()
  await expect(page.getByText('Disconnected', { exact: true })).toBeVisible()
  await expect(page.getByRole('log', { name: 'Container output' })).not.toContainText('synthetic late record')
})

test('all resource counters have recent history and missing samples stay unavailable', async ({ page }, testInfo) => {
  await mockRuntime(page)
  const history = [
    { read: '2026-01-01T00:00:01Z', cpuMillicores: 500, memoryBytes: 1024, pids: 2, networkRX: 1024, networkTX: 2048, blockRead: 3072, blockWrite: 4096 },
    { read: '2026-01-01T00:00:04Z', memoryBytes: 2048 },
    { read: '2026-01-01T00:00:07Z', cpuMillicores: 700, memoryBytes: 4096, pids: 3, networkRX: 2048, networkTX: 4096, blockRead: 6144, blockWrite: 8192 },
  ]
  await page.route(`**${containerPrefix}/stats`, (route) => route.fulfill({
    contentType: 'application/json', body: JSON.stringify({ available: true, history, current: history.at(-1) }),
  }))
  await openInspector(page, 'Stats')
  await page.getByText('Recent samples', { exact: true }).click()
  for (const [label, expected] of [
    ['CPU', '700m'], ['Memory', '4.0 KiB'], ['Processes', '3'], ['Network RX', '2.0 KiB'],
    ['Network TX', '4.0 KiB'], ['Block read', '6.0 KiB'], ['Block write', '8.0 KiB'],
  ]) {
    await page.getByLabel('History metric').selectOption({ label })
    await expect(page.getByRole('img', { name: `Recent ${label.toLowerCase()} history`, exact: true })).toBeVisible()
    const table = page.getByRole('table', { name: `Recent ${label.toLowerCase()} samples`, exact: true })
    await expect(table.getByRole('row')).toHaveCount(4)
    await expect(table.getByRole('cell', { name: expected, exact: true }).last()).toBeVisible()
    if (label !== 'Memory') await expect(table.getByRole('cell', { name: 'Unavailable', exact: true })).toHaveCount(1)
  }
  await page.screenshot({ path: testInfo.outputPath('container-stats.png'), fullPage: true })
})

test('stopped-container statistics expose retained samples and the backend limitation', async ({ page }) => {
  await mockRuntime(page, { ...snapshot, containers: [{ ...fixture, state: 'exited', status: 'Exited (0)', taskPresent: false }] })
  await page.route(`**${containerPrefix}/stats`, (route) => route.fulfill({
    contentType: 'application/json',
    body: JSON.stringify({ available: false, history: [{ read: '2026-01-01T00:00:00Z', memoryBytes: 1024 }], message: 'Synthetic task stopped; retained memory only.' }),
  }))
  await openInspector(page, 'Stats')
  await expect(page.getByText(/Live metrics require a running task/)).toBeVisible()
  await expect(page.getByText(/Synthetic task stopped/)).toBeVisible()
  await page.getByText('Recent samples', { exact: true }).click()
  await expect(page.getByRole('table', { name: 'Recent memory samples' })).toContainText('1.0 KiB')
})

for (const tab of ['Stats', 'Inspect']) {
  test(`${tab} cancels a pending request when the inspector closes`, async ({ page }) => {
    await mockRuntime(page)
    await probeRequests(page)
    await page.goto('/#/containers')
    await page.evaluate((path) => { window.fixtureHeldPath = path }, tab === 'Stats' ? `${containerPrefix}/stats` : containerPrefix)
    await page.getByText('fixture-api', { exact: true }).first().click()
    await page.getByRole('tab', { name: tab, exact: true }).click()
    await expect.poll(() => page.evaluate(() => window.fixtureRequests?.some((request) => request.path === window.fixtureHeldPath))).toBe(true)
    await page.getByRole('button', { name: 'Close inspector' }).click()
    await expect.poll(() => page.evaluate(() => window.fixtureRequests?.filter((request) => request.path === window.fixtureHeldPath).every((request) => request.aborted))).toBe(true)
  })
}

test('binary and large files can be downloaded without a text preview', async ({ page }, testInfo) => {
  await mockRuntime(page)
  await mockFiles(page)
  await openInspector(page, 'Files')
  for (const name of ['binary.bin', 'large.txt']) {
    const pending = page.waitForEvent('download')
    await page.getByRole('button', { name: `Download ${name}`, exact: true }).click()
    const download = await pending
    expect(download.suggestedFilename()).toBe(name)
    expect(await download.failure()).toBeNull()
  }
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveCount(0)
  await page.screenshot({ path: testInfo.outputPath('container-files.png'), fullPage: true })
})

test('file uploads create new files and reject implicit overwrites', async ({ page }) => {
  await mockRuntime(page)
  const requests = await mockFiles(page)
  await openInspector(page, 'Files')
  await page.getByLabel('Upload new file').setInputFiles({
    name: 'upload.fixture', mimeType: 'application/octet-stream', buffer: Buffer.from('synthetic upload'),
  })
  await expect(page.getByRole('button', { name: 'upload.fixture', exact: true })).toBeVisible()
  expect(requests.find((request) => request.method === 'PUT')).toMatchObject({
    path: 'upload.fixture', identity: 'synthetic-fingerprint', sha256: '', body: 'synthetic upload',
  })
  await page.getByLabel('Upload new file').setInputFiles({
    name: 'fixture.txt', mimeType: 'text/plain', buffer: Buffer.from('synthetic overwrite attempt'),
  })
  await expect(page.getByRole('alert').filter({ hasText: 'Overwrite requires a current checksum.' }).first()).toBeVisible()
  await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveValue('synthetic')
})

test('file edits and deletions require confirmation and the preview identity', async ({ page }) => {
  await mockRuntime(page)
  const requests = await mockFiles(page)
  await openInspector(page, 'Files')
  await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
  await page.getByRole('textbox', { name: 'File contents' }).fill('changed synthetic text')
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Save file', exact: true }).click()
  expect(requests.filter((request) => request.method === 'PUT')).toHaveLength(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Save file', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Save file', exact: true })).toBeDisabled()
  expect(requests.find((request) => request.method === 'PUT')).toMatchObject({
    path: 'fixture.txt', identity: 'synthetic-fingerprint', sha256: 'synthetic-checksum', body: 'changed synthetic text',
  })
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Delete file', exact: true }).click()
  expect(requests.filter((request) => request.method === 'DELETE')).toHaveLength(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Delete file', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveCount(0)
  expect(requests.find((request) => request.method === 'DELETE')).toMatchObject({
    path: 'fixture.txt', identity: 'synthetic-fingerprint', sha256: 'synthetic-checksum', confirm: 'true',
  })
})

test('selecting another file cannot silently discard an unsaved edit', async ({ page }) => {
  await mockRuntime(page)
  await mockFiles(page)
  await openInspector(page, 'Files')
  await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
  await page.getByRole('textbox', { name: 'File contents' }).fill('unsaved synthetic change')
  let prompted = false
  page.once('dialog', async (dialog) => { prompted = true; await dialog.dismiss() })
  await page.getByRole('button', { name: 'second.txt', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveValue('unsaved synthetic change')
  expect(prompted).toBe(true)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'second.txt', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveValue('second fixture')
})

test('text edit size is measured in UTF-8 bytes, not characters', async ({ page }) => {
  await mockRuntime(page)
  const requests = await mockFiles(page)
  await openInspector(page, 'Files')
  await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
  await page.getByRole('textbox', { name: 'File contents' }).fill('\u754c'.repeat(Math.floor(256 * 1024 / 3) + 1))
  await expect(page.getByText('Text edits are limited to 256 KiB of UTF-8 text.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Save file', exact: true })).toBeDisabled()
  expect(requests.filter((request) => request.method === 'PUT')).toHaveLength(0)
})

for (const action of ['preview', 'upload', 'download']) {
  test(`pending file ${action} cancels on close and can be explicitly canceled`, async ({ page }) => {
    await mockRuntime(page)
    await mockFiles(page)
    await probeRequests(page)
    await openInspector(page, 'Files')
    await expect(page.getByRole('button', { name: 'fixture.txt', exact: true })).toBeVisible()
    await page.evaluate((path) => { window.fixtureHeldPath = path }, `${containerPrefix}/file${action === 'download' ? '/download' : ''}`)
    async function start() {
      if (action === 'preview') await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
      if (action === 'upload') await page.getByLabel('Upload new file').setInputFiles({
        name: 'upload.fixture', mimeType: 'application/octet-stream', buffer: Buffer.from('synthetic upload'),
      })
      if (action === 'download') await page.getByRole('button', { name: 'Download binary.bin', exact: true }).click()
      await expect(page.getByRole('button', { name: 'Cancel file operation', exact: true })).toBeVisible()
    }
    await start()
    await page.getByRole('button', { name: 'Cancel file operation', exact: true }).click()
    await expect.poll(() => page.evaluate(() => window.fixtureRequests?.filter((request) => request.path === window.fixtureHeldPath).every((request) => request.aborted))).toBe(true)
    await start()
    await page.getByRole('button', { name: 'Close inspector' }).click()
    await expect.poll(() => page.evaluate(() => window.fixtureRequests?.filter((request) => request.path === window.fixtureHeldPath).every((request) => request.aborted))).toBe(true)
  })
}

for (const state of ['running', 'exited']) {
  test(`${state} container files use snapshots and respect mount boundaries`, async ({ page }) => {
    await mockRuntime(page, { ...snapshot, containers: [{ ...fixture, state, status: state, taskPresent: state === 'running' }] })
    await mockFiles(page)
    await openInspector(page, 'Files')
    await expect(page.getByText('Synthetic shell-independent container snapshot access.')).toBeVisible()
    await page.getByText('Mount boundaries', { exact: true }).click()
    await expect(page.getByText('volume · read-only · fixture-volume')).toBeVisible()
    await page.getByRole('button', { name: 'mounted', exact: true }).click()
    await expect(page.getByLabel('Directory path')).toHaveValue('mounted')
    await expect(page.getByLabel('Upload new file')).toBeDisabled()
    await page.getByRole('button', { name: 'config.txt', exact: true }).click()
    await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveAttribute('readonly', '')
    await expect(page.getByRole('button', { name: 'Delete file', exact: true })).toBeDisabled()
    await expect(page.getByRole('button', { name: 'Save file', exact: true })).toBeDisabled()
  })
}

test('read-only container files reject mutations and show unsupported capabilities explicitly', async ({ page }) => {
  await mockRuntime(page)
  await mockFiles(page, true)
  await openInspector(page, 'Files')
  await expect(page.getByLabel('Upload new file')).toBeDisabled()
  await page.getByRole('button', { name: 'fixture.txt', exact: true }).click()
  await expect(page.getByRole('textbox', { name: 'File contents' })).toHaveAttribute('readonly', '')
  await page.route(`**${containerPrefix}/files?*`, (route) => route.fulfill({
    status: 501, contentType: 'application/json', body: JSON.stringify({ message: 'Snapshot mounting unavailable; use the Porto-managed Lima engine.' }),
  }))
  await page.getByRole('button', { name: 'Refresh files', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Snapshot mounting unavailable' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Reconnect to current resource' })).toBeVisible()
})

test('file path and download failures remain visible and retryable', async ({ page }) => {
  await mockRuntime(page)
  await mockFiles(page)
  await openInspector(page, 'Files')
  await page.getByLabel('Directory path').fill('../outside')
  await page.getByRole('button', { name: 'Browse', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Invalid path' })).toBeVisible()
  await page.getByLabel('Directory path').fill('.')
  await page.getByRole('button', { name: 'Browse', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Download binary.bin', exact: true })).toBeVisible()
  await page.route(`**${containerPrefix}/file/download?*`, (route) => route.fulfill({
    status: 503, contentType: 'application/json', body: JSON.stringify({ message: 'Synthetic download connection lost. Reconnect and retry.' }),
  }))
  await page.getByRole('button', { name: 'Download binary.bin', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic download connection lost' }).first()).toBeVisible()
  await expect(page.getByRole('button', { name: 'Download binary.bin', exact: true })).toBeEnabled()
})
