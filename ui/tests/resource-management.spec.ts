import { expect, test, type Page } from '@playwright/test'
import type { AccelerationStatus } from '../src/acceleration'
import type { EngineResourceStatus, SaverStatus } from '../src/resourceManagement'

async function mockResources(page: Page) {
  const mutations: Array<{ path: string; method: string; body: unknown }> = []
  const saver: SaverStatus = {
    state: { config: { mode: 'disabled', idleSeconds: 300, wakeSeconds: 60, maxPending: 64, maxBodyBytes: 16777216, suspendEngine: false, exclusions: [], overrides: {} }, entries: {}, events: [], pending: 0 },
    applications: [], protocols: ['http', 'https'], rawTCP: false,
  }
  const acceleration: AccelerationStatus = {
    settings: { policy: 'automatic', rosettaEnabled: false },
    capabilities: { hostOS: 'darwin', hostArchitecture: 'arm64', guestArchitecture: 'arm64', vmType: 'vz', limaVersion: 'synthetic-lima',
      virtualization: { supported: true }, rosettaHost: { supported: true }, rosetta: { supported: false }, binfmt: { supported: true }, amd64Handler: 'qemu' },
  }
  const engine: EngineResourceStatus = { limits: { cpus: 4, memoryMiB: 4096, diskGiB: 20, swapMiB: null },
    supported: true, restartRequired: false, current: { cpus: 4, memory: 4294967296, disk: 21474836480, status: 'Running', vmType: 'vz' } }
  const shares: { supported: boolean; version: string; shares: unknown[] } = { supported: true, version: '0.18.1-porto.1', shares: [] }
  const settings = {
    cleanupLocalMerged: false, cleanupRemoteMerged: false, pruneRemoteTracking: false, protectedBranches: ['main'],
    sqlNotSoLiteEnabled: false, killSwitchEnabled: false, sendboxEnabled: false, dockerEnabled: true,
    dockerAutoPruneEnabled: false, kubernetesEnabled: false, vmsEnabled: true, theme: 'dark', interfaceDensity: 'compact',
    reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2, terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7,
  }
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const path = url.pathname
    const method = route.request().method()
    const json = (value: unknown) => route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
    if (method !== 'GET') {
      const body: unknown = route.request().postData() ? route.request().postDataJSON() : null
      mutations.push({ path: url.pathname + url.search, method, body })
      if (path === '/api/runtime/resource-saver') {
        saver.state.config = route.request().postDataJSON()
        return json(saver)
      }
      if (path === '/api/runtime/acceleration') {
        acceleration.settings = route.request().postDataJSON()
        return json(acceleration)
      }
      if (path === '/api/docker/engine/resources') {
        engine.limits = route.request().postDataJSON()
        engine.restartRequired = true
        return json(engine)
      }
      if (path === '/api/runtime/shares') {
        const share = { id: 'synthetic-share', projectId: 1, hostPath: '/synthetic/repository', guestPath: '/mnt/porto-sync/synthetic',
          mode: 'synchronized', status: 'watching', ignore: ['node_modules'], session: { paused: false, status: 'watching',
            alpha: { connected: true, scanned: true, files: 10000 }, beta: { connected: true, scanned: true, files: 10000 }, conflicts: [{ alphaChanges: ['synthetic host edit'], betaChanges: ['synthetic guest edit'] }] } }
        shares.shares = [share]
        return json(share)
      }
      if (path.startsWith('/api/runtime/shares/') && method === 'DELETE') {
        shares.shares = []
        return json({ status: 'detached' })
      }
      return json({ status: 'complete' })
    }
    const values: Record<string, unknown> = {
      '/api/settings': settings, '/api/health': { version: 'synthetic' },
      '/api/projects': [{ id: 1, name: 'Synthetic project', path: '/synthetic/repository', strategy: 'node', status: 'stopped', branch: 'main' }],
      '/api/runtime/features': { docker: true, kubernetes: false, vms: true },
      '/api/runtime/resource-saver': saver, '/api/runtime/acceleration': acceleration, '/api/docker/engine/resources': engine,
      '/api/runtime/shares': shares,
      '/api/docker/status': { enabled: true, available: true },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: true, runs: [] },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic', revision: 1, available: true, stale: false, containers: [], events: [], capabilities: {} },
      '/api/integrations/sql-not-so-lite': { state: 'disabled' }, '/api/integrations/sendbox': { state: 'disabled' },
      '/api/integrations/kill-switch': { supported: false, state: 'disabled', userPorts: [], syncedPorts: [], effectivePorts: [] },
      '/api/networking': { https: { supported: false }, proxy: { mode: 'off', registryCAs: [], credentialsStored: {} }, proxyAvailable: true, warnings: [], routes: [] },
      '/api/wsl': { supported: false, distributions: [], integrations: [] },
      '/api/docker/windows/status': { supported: false },
    }
    return json(values[path] ?? [])
  })
  await page.goto('/#/settings')
  return { mutations, saver, acceleration, engine, shares }
}

test('Resource Saver starts disabled and saves explicit bounded policies without applying unrelated changes', async ({ page }) => {
  const fixture = await mockResources(page)
  const region = page.getByRole('region', { name: 'Resource Saver', exact: true })
  await expect(region.getByLabel('Automatic idle action')).toHaveValue('disabled')
  await region.getByLabel('Automatic idle action').selectOption('container-scale-to-zero')
  await region.getByLabel('Idle timeout (seconds)').fill('600')
  await region.getByLabel('Wake deadline (seconds)').fill('90')
  await region.getByLabel('Excluded application/container names').fill('database-*, important-api')
  await region.getByLabel('Per-application overrides (JSON)').fill('{"compose:synthetic":{"idleSeconds":900}}')
  await region.getByRole('button', { name: 'Save Resource Saver policy' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(1)
  expect(fixture.mutations[0]).toMatchObject({ path: '/api/runtime/resource-saver', method: 'PUT',
    body: { mode: 'container-scale-to-zero', idleSeconds: 600, wakeSeconds: 90, maxPending: 64, exclusions: ['database-*', 'important-api'], overrides: { 'compose:synthetic': { idleSeconds: 900 } } } })
  expect(fixture.mutations.some((request) => request.path.endsWith('/apply'))).toBe(false)
})

test('Rosetta stays opt-in and applying its engine policy requires separate confirmation', async ({ page }) => {
  const fixture = await mockResources(page)
  const region = page.getByRole('region', { name: 'Architecture and acceleration', exact: true })
  await expect(region.getByLabel('Use Rosetta in the owned engine')).not.toBeChecked()
  await region.getByLabel('Architecture policy').selectOption('rosetta-preferred')
  await region.getByLabel('Use Rosetta in the owned engine').check()
  await region.getByRole('button', { name: 'Save architecture policy' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(1)
  expect(fixture.mutations[0]).toMatchObject({ body: { policy: 'rosetta-preferred', rosettaEnabled: true } })
  page.once('dialog', (dialog) => dialog.dismiss())
  await region.getByRole('button', { name: 'Apply to idle engine' }).click()
  expect(fixture.mutations).toHaveLength(1)
  page.once('dialog', (dialog) => dialog.accept())
  await region.getByRole('button', { name: 'Apply to idle engine' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(2)
  expect(fixture.mutations[1]).toMatchObject({ path: '/api/runtime/acceleration/apply', body: { restartEngine: true } })
})

test('CPU memory swap and disk budgets remain pending until an explicit idle restart', async ({ page }) => {
  const fixture = await mockResources(page)
  const region = page.getByRole('region', { name: 'Engine resource budgets', exact: true })
  await region.getByLabel('Virtual CPUs').fill('2')
  await region.getByLabel('Memory (MiB)', { exact: true }).fill('2048')
  await region.getByLabel('Managed guest swap (MiB)').fill('0')
  await region.getByLabel('Disk (GiB)', { exact: true }).fill('40')
  await region.getByRole('button', { name: 'Save engine budgets' }).click()
  await expect(region.getByText('Saved budgets are pending an engine restart.')).toBeVisible()
  expect(fixture.mutations[0]).toMatchObject({ path: '/api/docker/engine/resources', body: { cpus: 2, memoryMiB: 2048, swapMiB: 0, diskGiB: 40 } })
  expect(fixture.mutations).toHaveLength(1)
})

test('optimized sharing selects one registered root and displays conflicts without discarding either copy', async ({ page }) => {
  const fixture = await mockResources(page)
  const region = page.getByRole('region', { name: 'Optimized project shares', exact: true })
  await region.getByLabel('Registered project root').selectOption('1')
  await region.getByLabel('Additional ignore patterns').fill('node_modules, dist')
  await region.getByRole('button', { name: 'Enable selected project share' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(1)
  expect(fixture.mutations[0]).toMatchObject({ path: '/api/runtime/shares', body: { projectId: 1, ignore: ['node_modules', 'dist'] } })
  await expect(region.getByRole('alert').filter({ hasText: 'unresolved conflict(s)' })).toBeVisible()
  await region.getByText('Conflict details', { exact: true }).click()
  await expect(region.getByText(/synthetic host edit/)).toBeVisible()
  await expect(region.getByText(/synthetic guest edit/)).toBeVisible()
  page.once('dialog', (dialog) => dialog.dismiss())
  await region.getByRole('button', { name: 'Detach; keep both copies' }).click()
  expect(fixture.mutations).toHaveLength(1)
  page.once('dialog', (dialog) => dialog.accept())
  await region.getByRole('button', { name: 'Detach; keep both copies' }).click()
  await expect.poll(() => fixture.mutations.length).toBe(2)
  expect(fixture.mutations[1]).toMatchObject({ path: '/api/runtime/shares/synthetic-share?confirm=true', method: 'DELETE' })
})
