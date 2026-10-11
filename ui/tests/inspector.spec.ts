import { expect, test, type Page } from '@playwright/test'

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
  }
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

test('logs remain bounded, searchable, stream-filtered and close on tab switch', async ({ page }) => {
      await mockRuntime(page)
      await page.addInitScript(() => {
        const Original = window.EventSource
        Object.defineProperty(window, 'EventSource', { value: function (url: string) {
          if (!String(url).includes('/logs/stream')) return new Original(url)
          const target = new EventTarget()
          const source = { target, closed: false }
          window.fixtureLogSources ??= []
          window.fixtureLogSources.push(source)
          return Object.assign(target, {
            url, readyState: 1, onopen: null, onerror: null,
            close() { source.closed = true },
          })
        } })
      })
      await page.goto('/#/containers')
      await page.getByText('fixture-api', { exact: true }).first().click()
      await page.getByRole('tab', { name: 'Logs', exact: true }).click()
      await expect.poll(() => page.evaluate(() => window.fixtureLogSources?.length ?? 0)).toBeGreaterThan(0)
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
