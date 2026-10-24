import { expect, test, type Page } from '@playwright/test'

async function mockPlatform(page: Page) {
  page.on('pageerror', (error) => { throw error })
  const mutations: Array<{ path: string; body: unknown }> = []
  const network = {
    platform: 'windows',
    https: { supported: true, installed: false, listening: false, trusted: false },
    proxyAvailable: true,
    proxy: { mode: 'system', httpProxy: '', httpsProxy: '', socksProxy: '', noProxy: '', registryCAs: [], source: 'environment', credentialsStored: {} },
    routes: [], warnings: [], engine: { configured: false, needsApply: false, backend: 'lima' }, engineError: '',
  }
  const distributions = [
    { name: 'Synthetic Ubuntu \u03b4', id: 'synthetic-selected', version: 2, running: false, enabled: false },
    { name: 'Synthetic Debian', id: 'synthetic-unselected', version: 2, running: false, enabled: false },
    { name: 'Synthetic legacy', id: 'synthetic-legacy', version: 1, running: false, enabled: false },
  ]
  const integration = { state: 'disabled', message: 'Synthetic integration disabled.', updatedAt: '2026-01-01T00:00:00Z' }
  let failHTTPS = false
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const method = route.request().method()
    const json = (body: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(body) })
    if (method !== 'GET') {
      const body: unknown = route.request().postData() ? route.request().postDataJSON() : null
      mutations.push({ path, body })
      if (path === '/api/tls/https/install') {
        if (failHTTPS) return json({ message: 'Synthetic elevation denied. No integration was installed.' }, 403)
        network.https = { supported: true, installed: true, listening: true, trusted: true }
        return json(network)
      }
      if (path === '/api/networking/proxy') {
        network.proxy = {
          ...network.proxy,
          mode: 'manual', httpsProxy: 'http://proxy.example:3128', source: 'manual',
        }
        network.engine = { configured: true, needsApply: true, backend: 'lima' }
        return json(network.proxy)
      }
      if (path.endsWith('/enable')) {
        distributions[0].enabled = true
        return json({ message: 'Selected tooling enabled.' })
      }
      return json({ message: 'Synthetic operation complete.' })
    }
    const values: Record<string, unknown> = {
      '/api/settings': {
        cleanupLocalMerged: false, cleanupRemoteMerged: false, pruneRemoteTracking: false,
        protectedBranches: ['main'], sqlNotSoLiteEnabled: false, killSwitchEnabled: false,
        sendboxEnabled: false, dockerEnabled: true, dockerAutoPruneEnabled: false,
        kubernetesEnabled: false, vmsEnabled: false, theme: 'dark', interfaceDensity: 'compact',
        reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2,
        terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7,
      },
      '/api/health': { version: '1.4.0' },
      '/api/integrations/sql-not-so-lite': integration,
      '/api/integrations/sendbox': integration,
      '/api/integrations/kill-switch': { ...integration, supported: false, installed: false, autoKillEnabled: null, userPorts: [], syncedPorts: [], effectivePorts: [] },
      '/api/docker/status': { enabled: true, available: true },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: true, runs: [] },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic-platform', revision: 1, available: true, stale: false, containers: [], events: [], capabilities: {} },
      '/api/networking': network,
      '/api/wsl': { supported: true, distributions, integrations: [] },
      '/api/networking/connectivity': {
        host: { subnets: [], localhostAddresses: ['127.0.0.1', '::1'], warnings: ['Synthetic split-DNS policy needs inspection.'] },
        containerDirectIP: { supported: false, message: 'Rootless container addresses stay private. Use published loopback ports.' },
        networks: [], virtualMachines: [], containers: [],
        vmDirectIP: { supported: false, message: 'QEMU uses owned loopback forwards.' },
      },
    }
    return json(values[path] ?? [])
  })
  await page.goto('/#/settings')
  return { mutations, denyHTTPS: () => { failHTTPS = true } }
}

test('HTTPS setup requests explicit consent and shows native trust state', async ({ page }, testInfo) => {
  const fixture = await mockPlatform(page)
  await expect(page.getByRole('heading', { name: 'Trusted local HTTPS' })).toBeVisible()
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Install trusted HTTPS' }).click()
  expect(fixture.mutations).toHaveLength(0)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Install trusted HTTPS' }).click()
  await expect(page.getByText('Installed, listening, and trusted.', { exact: true })).toBeVisible()
  expect(fixture.mutations[0].path).toBe('/api/tls/https/install')
  await page.getByRole('region', { name: 'Trusted local HTTPS', exact: true }).screenshot({ path: testInfo.outputPath('networking-settings.png'), animations: 'disabled' })
})

test('proxy secrets are cleared after save and engine application stays explicit', async ({ page }) => {
  const fixture = await mockPlatform(page)
  await page.getByLabel('Proxy mode').selectOption('manual')
  await page.getByLabel('HTTPS proxy URL').fill('http://synthetic:mock-password@proxy.example:3128')
  await page.getByRole('button', { name: 'Save network settings' }).click()
  await expect(page.getByLabel('HTTPS proxy URL')).toHaveValue('http://proxy.example:3128')
  expect(fixture.mutations[0]).toMatchObject({
    path: '/api/networking/proxy',
    body: { mode: 'manual', httpsProxy: 'http://synthetic:mock-password@proxy.example:3128' },
  })
  expect(fixture.mutations.some((request) => request.path.endsWith('/apply'))).toBe(false)
  expect(await page.evaluate(() => JSON.stringify(localStorage))).not.toContain('mock-password')
})

test('WSL operations affect only the selected distribution and never restart implicitly', async ({ page }, testInfo) => {
  const fixture = await mockPlatform(page)
  await expect(page.getByRole('button', { name: 'Enable Synthetic legacy', exact: true })).toBeDisabled()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Enable Synthetic Ubuntu \u03b4', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Repair Synthetic Ubuntu \u03b4', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Enable Synthetic Debian', exact: true })).toBeVisible()
  expect(fixture.mutations).toHaveLength(1)
  expect(decodeURIComponent(fixture.mutations[0].path)).toBe('/api/wsl/distributions/Synthetic Ubuntu \u03b4/enable')
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Apply WSL mounts' }).click()
  expect(fixture.mutations).toHaveLength(1)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Apply WSL mounts' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(2)
  expect(fixture.mutations[1]).toMatchObject({ path: '/api/wsl/mounts', body: { restartEngine: true } })
  await page.getByRole('region', { name: 'Selected WSL distributions', exact: true }).screenshot({ path: testInfo.outputPath('wsl-settings.png'), animations: 'disabled' })
})

test('denied HTTPS elevation stays actionable and connectivity limits fit narrow screens', async ({ page }) => {
  const fixture = await mockPlatform(page)
  fixture.denyHTTPS()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Install trusted HTTPS' }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic elevation denied.' }).first()).toBeVisible()
  await page.getByRole('button', { name: 'Inspect connectivity' }).click()
  await expect(page.getByText('Rootless container addresses stay private. Use published loopback ports.')).toBeVisible()
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})
