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

async function mockRuntime(page: Page) {
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const values: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false },
      '/api/docker/status': { available: true, enabled: true, context: 'porto' },
      '/api/docker/cleanup': { runs: [], enabled: false, dockerEnabled: true },
      '/api/docker/containers/snapshot': snapshot,
      '/api/docker/containers': [fixture],
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
      await route.fulfill({ contentType: 'text/event-stream', body: `event: snapshot\ndata: ${JSON.stringify(snapshot)}\n\n` })
      return
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
}

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
