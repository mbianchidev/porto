import { expect, test, type Page } from '@playwright/test'

const clusterID = '18446744073709551001'
const instance = {
  format: 1, name: 'synthetic-datastore', clusterID, state: 'stopped', message: 'Synthetic members stopped.',
  desiredRunning: false, createdAt: '2026-01-01T00:00:00Z',
  members: [{
    name: 'member-1', id: '18446744073709551002', initialized: true,
    clientURL: 'https://host.lima.internal:52479', peerURL: 'https://127.0.0.1:52480',
    listen: '0.0.0.0:52479', peerListen: '127.0.0.1:52480', state: 'stopped',
  }],
}

async function mockEtcqlite(page: Page, enabled = false) {
  const requests: Array<{ path: string; method: string; body: unknown }> = []
  let settings = {
    protectedBranches: ['main'], dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false,
    etcqliteEnabled: enabled, theme: 'dark', interfaceDensity: 'compact', logRetentionDays: 7,
  }
  let members = enabled ? [structuredClone(instance)] : []
  let failBackup = false
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    const body: unknown = request.postData() ? request.postDataJSON() : null
    requests.push({ path, method: request.method(), body })
    const reply = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) })
    if (request.method() === 'PUT' && path === '/api/settings') {
      settings = { ...settings, ...request.postDataJSON() }
      return reply(settings)
    }
    if (request.method() === 'POST' && path === '/api/integrations/etcqlite/instances') {
      members = [{ ...structuredClone(instance), name: request.postDataJSON().name }]
      return reply(members[0], 201)
    }
    if (request.method() !== 'GET' && path.startsWith('/api/integrations/etcqlite/instances/')) {
      if (path.endsWith('/backup') && failBackup) return reply({ message: 'Synthetic native integrity failure. Previous verified backup is retained.' }, 500)
      if (path.endsWith('/start')) members = members.map((member) => ({ ...member, state: 'ready' }))
      if (request.method() === 'DELETE') members = []
      return reply({ path: '/synthetic/private-native-backup', message: 'Synthetic explicit lifecycle completed.' })
    }
    if (request.method() === 'POST' && path === '/api/kubernetes/clusters') {
      return reply({ name: request.postDataJSON().name, provider: 'k3s' }, 201)
    }
    const integration = { state: 'disabled', message: 'Synthetic integration disabled.' }
    const values: Record<string, unknown> = {
      '/api/health': { version: '1.7.3' },
      '/api/settings': settings,
      '/api/integrations/etcqlite': { supported: true, state: settings.etcqliteEnabled ? 'configured' : 'disabled', message: 'Synthetic explicit datastore state.', instances: members },
      '/api/integrations/etcqlite/backups': [],
      '/api/integrations/sql-not-so-lite': integration,
      '/api/integrations/sendbox': integration,
      '/api/integrations/kill-switch': { ...integration, supported: false, syncedPorts: [], installed: false },
      '/api/docker/status': { enabled: false, available: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
      '/api/kubernetes/status': { enabled: true, available: true },
      '/api/kubernetes/contexts': [],
      '/api/kubernetes/clusters': [],
      '/api/runtime/providers': [],
    }
    return reply(values[path] ?? [])
  })
  return { requests, ready: () => { members = [{ ...structuredClone(instance), state: 'ready' }] }, failBackup: () => { failBackup = true } }
}

test('Etcqlite is opt-in and saving its gate never creates or starts members', async ({ page }) => {
  const fixture = await mockEtcqlite(page)
  await page.goto('/#/settings')
  const panel = page.getByRole('region', { name: 'Optional Etcqlite datastore', exact: true })
  await expect(panel.getByLabel('Enable Etcqlite')).not.toBeChecked()
  await expect(panel.getByRole('button', { name: 'Initialize Etcqlite', exact: true })).toBeDisabled()
  expect(fixture.requests.some((request) => request.path === '/api/integrations/etcqlite/backups')).toBe(false)
  await panel.getByLabel('Enable Etcqlite').check()
  expect(fixture.requests.filter((request) => request.method !== 'GET')).toHaveLength(0)
  await panel.getByRole('button', { name: 'Save integration setting', exact: true }).click()
  await expect.poll(() => fixture.requests.filter((request) => request.method === 'PUT').length).toBe(1)
  expect(fixture.requests.some((request) => request.method !== 'GET' && request.path.startsWith('/api/integrations/etcqlite/'))).toBe(false)
  await expect(panel.getByRole('button', { name: 'Initialize Etcqlite', exact: true })).toBeDisabled()
})

test('initialization, startup and backup are separate identity-bound actions', async ({ page }) => {
  const fixture = await mockEtcqlite(page, true)
  await page.goto('/#/settings')
  const panel = page.getByRole('region', { name: 'Optional Etcqlite datastore', exact: true })
  await panel.getByLabel('Etcqlite name').fill('synthetic-new')
  await panel.getByLabel('Advertised client hostname').fill('host.lima.internal')
  await panel.getByLabel('Client listen address').fill('0.0.0.0')
  await panel.getByRole('button', { name: 'Initialize Etcqlite', exact: true }).click()
  await expect(panel.getByText('synthetic-new', { exact: true })).toBeVisible()
  const created = fixture.requests.filter((request) => request.method === 'POST')
  expect(created).toHaveLength(1)
  expect(created[0]).toMatchObject({
    path: '/api/integrations/etcqlite/instances',
    body: { name: 'synthetic-new', members: 1, advertiseHost: 'host.lima.internal', listenHost: '0.0.0.0' },
  })
  await panel.getByRole('button', { name: 'Start synthetic-new', exact: true }).click()
  await expect.poll(() => fixture.requests.some((request) => request.path.endsWith('/synthetic-new/start'))).toBe(true)
  expect(fixture.requests.find((request) => request.path.endsWith('/synthetic-new/start'))?.body).toEqual({ clusterID })
  fixture.failBackup()
  await panel.getByRole('button', { name: 'Back up synthetic-new', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic native integrity failure.' }).first()).toBeVisible()
  expect(fixture.requests.some((request) => request.method === 'DELETE')).toBe(false)
})

test('removal requires confirmation and preserves full uint64 identity', async ({ page }) => {
  const fixture = await mockEtcqlite(page, true)
  await page.goto('/#/settings')
  const panel = page.getByRole('region', { name: 'Optional Etcqlite datastore', exact: true })
  page.once('dialog', (dialog) => dialog.dismiss())
  await panel.getByRole('button', { name: 'Retain and remove synthetic-datastore', exact: true }).click()
  expect(fixture.requests.some((request) => request.method === 'DELETE')).toBe(false)
  page.once('dialog', (dialog) => dialog.accept())
  await panel.getByRole('button', { name: 'Retain and remove synthetic-datastore', exact: true }).click()
  await expect.poll(() => fixture.requests.some((request) => request.method === 'DELETE')).toBe(true)
  expect(fixture.requests.find((request) => request.method === 'DELETE')?.body).toEqual({ clusterID, confirm: true })
})

test('new k3s defaults stay unchanged until a datastore is explicitly selected', async ({ page }) => {
  const fixture = await mockEtcqlite(page, true)
  fixture.ready()
  await page.goto('/#/kubernetes')
  await page.getByRole('button', { name: 'New cluster', exact: true }).click()
  await expect(page.getByLabel('k3s datastore')).toHaveValue('')
  await page.getByLabel('Name', { exact: true }).fill('synthetic-default')
  await page.getByRole('button', { name: 'Create cluster', exact: true }).click()
  await expect.poll(() => fixture.requests.filter((request) => request.path === '/api/kubernetes/clusters' && request.method === 'POST').length).toBe(1)
  expect(fixture.requests.find((request) => request.path === '/api/kubernetes/clusters' && request.method === 'POST')?.body).not.toHaveProperty('etcqlite')
  await page.getByLabel('Name', { exact: true }).fill('synthetic-opt-in')
  await page.getByLabel('k3s datastore').selectOption('synthetic-datastore')
  await page.getByRole('button', { name: 'Create cluster', exact: true }).click()
  await expect.poll(() => fixture.requests.filter((request) => request.path === '/api/kubernetes/clusters' && request.method === 'POST').length).toBe(2)
  expect(fixture.requests.filter((request) => request.path === '/api/kubernetes/clusters' && request.method === 'POST')[1].body).toHaveProperty('etcqlite', { name: 'synthetic-datastore', clusterID })
  await page.getByRole('combobox', { name: /^Provider/ }).selectOption('k0s')
  await expect(page.getByLabel('k3s datastore')).toHaveCount(0)
})
