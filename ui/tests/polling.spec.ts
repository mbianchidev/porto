import { expect, test, type Page } from '@playwright/test'

async function mockDashboard(page: Page) {
  const requests = new Map<string, number>()
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    requests.set(path, (requests.get(path) ?? 0) + 1)
    const values: Record<string, unknown> = {
      '/api/environments': [{
        id: 'local', name: 'This machine', backend: 'local', address: 'loopback', identity: 'synthetic-polling',
        capabilities: [], labels: {}, state: 'online', revision: 1, revoked: false, remoteRevoked: false, owned: true,
      }],
      '/api/settings': { dockerEnabled: false, kubernetesEnabled: false, vmsEnabled: false, theme: 'dark', protectedBranches: [] },
      '/api/docker/status': { available: false, enabled: false, context: 'synthetic-polling' },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic-polling', revision: 1, available: false, stale: false, containers: [], capabilities: {} },
      '/api/kubernetes/status': { available: false, enabled: false },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
  return (path: string) => requests.get(path) ?? 0
}

async function setVisibility(page: Page, state: DocumentVisibilityState) {
  await page.evaluate((value) => {
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value })
    document.dispatchEvent(new Event('visibilitychange'))
  }, state)
}

const polled = ['/api/projects', '/api/environments', '/api/integrations/kill-switch']

test('dashboard polling pauses while the window is hidden and refreshes when shown', async ({ page }) => {
  await page.clock.install()
  const requestCount = await mockDashboard(page)
  const counts = () => Object.fromEntries(polled.map((path) => [path, requestCount(path)]))
  await page.goto('/#/localhost-ing')
  await expect.poll(() => requestCount('/api/projects')).toBeGreaterThan(0)

  const visible = requestCount('/api/projects')
  await page.clock.fastForward(5000)
  await expect.poll(() => requestCount('/api/projects')).toBeGreaterThan(visible)

  await setVisibility(page, 'hidden')
  // Let requests that started before the page was hidden reach the mock.
  await page.waitForTimeout(500)
  const hidden = counts()
  await page.clock.fastForward(60000)
  await page.waitForTimeout(500)
  expect(counts()).toEqual(hidden)

  await setVisibility(page, 'visible')
  await page.clock.fastForward(1)
  for (const path of polled) {
    await expect.poll(() => requestCount(path)).toBeGreaterThan(hidden[path])
  }
  expect(await page.pageErrors()).toEqual([])
})
