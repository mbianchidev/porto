import { expect, test, type Page } from '@playwright/test'

async function mockWindows(page: Page, supported = true, ready = false) {
  page.on('pageerror', error => { throw error })
  const mutations: string[] = []
  const requests: Array<{ path: string; mode: string | undefined; query: string | null }> = []
  const status = {
    supported, installed: ready, running: ready, owned: ready,
    context: 'porto-windows', endpoint: 'npipe:////./pipe/porto-windows-synthetic',
    host: { os: 'windows', architecture: supported ? 'amd64' : 'arm64', build: 26100, edition: 'ServerDatacenter', server: true, containers: true, hyperV: false, virtualization: false },
    message: supported ? 'Native runtime is not installed.' : 'Native Windows containers require amd64. Linux and WSL ARM64 remain supported.',
    capabilities: { process: { supported, reason: '' }, hyperv: { supported: false, reason: 'Hyper-V is not verified.' } },
  }
  let deny = false
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url())
    const path = url.pathname
    const mode = route.request().headers()['x-porto-container-mode']
    requests.push({ path, mode, query: url.searchParams.get('containerMode') })
    const json = (body: unknown, code = 200) => route.fulfill({ status: code, contentType: 'application/json', body: JSON.stringify(body) })
    if (route.request().method() !== 'GET') {
      mutations.push(path)
      if (deny) return json({ message: 'Synthetic native setup permission denied. Existing Linux data was preserved.' }, 403)
      if (path.endsWith('/install')) Object.assign(status, { installed: true, running: true, owned: true, message: 'Native runtime ready.' })
      return json(status)
    }
    const native = mode === 'windows' || url.searchParams.get('containerMode') === 'windows'
    const container = {
      id: native ? 'native-synthetic' : 'linux-synthetic', name: native ? 'synthetic-windows-app' : 'synthetic-linux-app',
      image: native ? 'synthetic-windows:latest' : 'synthetic-linux:latest', command: '', state: 'running', status: 'Up',
      ports: '', networks: '', mounts: '', createdAt: '', taskPresent: true, health: { status: 'disabled', failingStreak: 0 }, resources: {},
    }
    const snapshot = { instanceId: native ? 'native-runtime' : 'linux-runtime', revision: 1, available: true, stale: false, containers: [container], events: [], capabilities: {
      directCreation: { supported: true }, execLifecycle: { supported: true }, healthUpdates: { supported: false },
      networkUpdates: { supported: true }, checkpointRestore: { supported: false },
    } }
    if (path === '/api/docker/containers/events') {
      return route.fulfill({ contentType: 'text/event-stream', body: `event: snapshot\ndata: ${JSON.stringify(snapshot)}\n\n` })
    }
    const values: Record<string, unknown> = {
      '/api/settings': { protectedBranches: ['main'], dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false, theme: 'dark', interfaceDensity: 'compact', reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2, terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7 },
      '/api/health': { version: '1.4.0' },
      '/api/integrations/sql-not-so-lite': { state: 'disabled', message: 'Synthetic integration disabled.' },
      '/api/integrations/sendbox': { state: 'disabled', message: 'Synthetic integration disabled.' },
      '/api/integrations/kill-switch': { state: 'disabled', supported: false, installed: false, userPorts: [], syncedPorts: [], effectivePorts: [], message: 'Synthetic integration disabled.' },
      '/api/integrations/windows-containers': status,
      '/api/docker/status': { enabled: true, available: true, backend: native ? 'Windows containerd/runhcs' : 'Lima Linux' },
      '/api/docker/containers/snapshot': snapshot,
      '/api/docker/cleanup': { enabled: false, dockerEnabled: true, runs: [] },
      '/api/wsl': { supported: true, distributions: [], integrations: [] },
    }
    return json(values[path] ?? [])
  })
  await page.goto('/#/settings')
  return { mutations, requests, status, deny: () => { deny = true } }
}

test('native setup requires consent and never mutates the Linux engine', async ({ page }) => {
  const fixture = await mockWindows(page)
  await expect(page.getByRole('heading', { name: 'Native Windows containers', exact: true })).toBeVisible()
  page.once('dialog', dialog => dialog.dismiss())
  await page.getByRole('button', { name: 'Install native Windows runtime', exact: true }).click()
  expect(fixture.mutations).toHaveLength(0)
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: 'Install native Windows runtime', exact: true }).click()
  await expect(page.getByRole('button', { name: 'View Windows containers', exact: true })).toBeEnabled()
  expect(fixture.mutations).toEqual(['/api/integrations/windows-containers/install'])
})

test('switching views isolates cached Linux and Windows inventories and live URLs', async ({ page }) => {
  const fixture = await mockWindows(page, true, true)
  await page.goto('/#/containers')
  await expect(page.getByText('synthetic-linux-app', { exact: true })).toBeVisible()
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('combobox', { name: 'Container backend view', exact: true }).selectOption('windows')
  await expect(page.getByText('synthetic-windows-app', { exact: true })).toBeVisible()
  await expect(page.getByText('synthetic-linux-app', { exact: true })).toHaveCount(0)
  expect(fixture.requests.some(request => request.path === '/api/docker/containers/snapshot' && request.mode === 'windows')).toBe(true)
  expect(fixture.requests.some(request => request.path === '/api/docker/containers/events' && request.query === 'windows')).toBe(true)
  await page.goto('/#/builds')
  await expect(page.getByRole('heading', { name: 'Windows image builds' })).toBeVisible()
  await page.reload()
  await expect(page.getByRole('heading', { name: 'Windows image builds' })).toBeVisible()
  expect(fixture.requests.some(request => request.path === '/api/docker/builds')).toBe(false)
  await page.goto('/#/containers')
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('combobox', { name: 'Container backend view', exact: true }).selectOption('linux')
  await expect(page.getByText('synthetic-linux-app', { exact: true })).toBeVisible()
  await expect(page.getByText('synthetic-windows-app', { exact: true })).toHaveCount(0)
  expect(fixture.mutations).toHaveLength(0)
})

test('unsupported architecture is explicit without x64 substitution', async ({ page }) => {
  await mockWindows(page, false)
  await expect(page.getByText(/Native Windows containers require amd64/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Install native Windows runtime', exact: true })).toBeDisabled()
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('denied setup preserves the unavailable state', async ({ page }) => {
  const fixture = await mockWindows(page)
  fixture.deny()
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('button', { name: 'Install native Windows runtime', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic native setup permission denied' }).first()).toBeVisible()
  await expect(page.getByRole('button', { name: 'View Windows containers', exact: true })).toBeDisabled()
})

test('native terminals use Windows shells and an explicitly scoped WebSocket', async ({ page }) => {
  let connected = ''
  await page.routeWebSocket('**/api/docker/containers/native-synthetic/terminal*', socket => {
    connected = socket.url()
    socket.send(Buffer.from('synthetic Windows console\r\n'))
  })
  await mockWindows(page, true, true)
  await page.goto('/#/containers')
  page.once('dialog', dialog => dialog.accept())
  await page.getByRole('combobox', { name: 'Container backend view', exact: true }).selectOption('windows')
  await page.getByRole('option', { name: /synthetic-windows-app/ }).click()
  await page.getByRole('tab', { name: 'Terminal', exact: true }).click()
  await expect(page.getByRole('combobox', { name: 'Shell', exact: true })).toHaveValue('cmd.exe')
  await expect(page.getByRole('button', { name: 'Debug toolbox', exact: true })).toBeDisabled()
  await expect.poll(() => connected).toContain('containerMode=windows')
  expect(new URL(connected).searchParams.get('shell')).toBe('cmd.exe')
})
