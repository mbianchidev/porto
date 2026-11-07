import { expect, test, type Page } from '@playwright/test'
import type { BackupSchedule, DataOperation, DataRequest, FileResource, StorageResource, StorageUsage } from '../src/dataTypes'

const resource: FileResource = {
  kind: 'volume', name: 'synthetic-data', id: 'synthetic-volume-1', backend: 'synthetic/default', readOnly: false,
}
const volume: StorageResource = {
  resource, identity: 'synthetic-identity', logicalBytes: 2048, allocatedBytes: 4096,
  inUse: true, dangling: false, protected: false,
  owners: [{
    id: 'synthetic-container', name: 'synthetic-database', state: 'running',
    composeProject: 'synthetic-stack', composeService: 'database', managed: false,
    mounts: [{ path: '/data', kind: 'volume', source: '/synthetic/volume/_data', readOnly: false }],
  }],
}
const cache: StorageResource = {
  resource: { kind: 'cache', name: 'synthetic-cache', id: 'synthetic-cache', readOnly: false },
  identity: 'synthetic-cache-identity', logicalBytes: 1024, allocatedBytes: 1024,
  inUse: false, dangling: false, protected: false, owners: [],
  description: 'Synthetic build copy', cacheType: 'regular', usageCount: 3,
}
const usage: StorageUsage = {
  resources: [volume, cache], contentBytes: 8192, snapshotBytes: 16384, writableBytes: 2048,
  volumeBytes: 4096, buildCacheBytes: 1024, metadataBytes: 32768,
  warnings: ['Metadata covers all namespaces.'], accounting: 'Unique content and snapshots; no double-counted total.',
}
const visualUsage: StorageUsage = {
  ...usage,
  resources: [
    volume,
    {
      ...cache, logicalBytes: 1024 ** 3, allocatedBytes: 512 * 1024 ** 2, sharedBytes: 256 * 1024 ** 2,
      description: 'Synthetic build context for the local development stack',
    },
    {
      resource: {
        kind: 'image', name: `registry.synthetic.invalid/development/${'shared-application-'.repeat(6)}:fixture`,
        id: 'synthetic-protected-image', readOnly: true,
      },
      identity: 'synthetic-protected-image-identity', logicalBytes: 3 * 1024 ** 3,
      allocatedBytes: 2 * 1024 ** 3, sharedBytes: 1024 ** 3,
      inUse: false, dangling: false, protected: true, owners: [],
      reason: 'Managed Kubernetes content remains protected even when no container is running.',
    },
    {
      resource: { kind: 'network', name: 'synthetic-unused-network', id: 'synthetic-network', readOnly: false },
      identity: 'synthetic-network-identity', inUse: false, dangling: true, protected: false, owners: [],
    },
  ],
  contentBytes: 8 * 1024 ** 3, snapshotBytes: 16 * 1024 ** 3, writableBytes: 2 * 1024 ** 3,
  volumeBytes: 4 * 1024 ** 3, buildCacheBytes: 512 * 1024 ** 2, metadataBytes: 32 * 1024 ** 2,
}
const schedule: BackupSchedule = {
  id: 1, resource, enabled: true, intervalHours: 24, retention: 7,
  directory: '/synthetic/backups/data', nextRunAt: '2026-01-02T00:00:00Z',
}

async function mockStorage(page: Page, schedules: BackupSchedule[] = [], initialUsage = usage) {
  const requests: DataRequest[] = []
  const saves: BackupSchedule[] = []
  const deleted: string[] = []
  let operations: DataOperation[] = []
  let currentSchedules = schedules
  let currentUsage = initialUsage
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const method = route.request().method()
    const json = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) })
    if (path === '/api/docker/storage/preview') {
      const request = route.request().postDataJSON() as DataRequest
      requests.push(request)
      if (request.action === 'prune') {
        return json({
          request: { action: 'prune', selections: [{ kind: 'cache', name: 'synthetic-cache', id: 'synthetic-cache' }], preview: 'synthetic-prune' },
          candidates: [cache], excluded: [volume], upperBoundBytes: 1024, token: 'synthetic-prune',
          message: 'Only exact previewed resources can be removed; reclaimed bytes are not guaranteed.',
        })
      }
      return json({
        request: { ...request, resource, identity: volume.identity, preview: 'synthetic-volume-preview' },
        resource, owners: volume.owners, destination: request.destination || (request.action === 'volume-remove' ? 'permanently removed' : ''),
        token: 'synthetic-volume-preview', consistency: 'crash-consistent',
        consequences: request.action === 'volume-remove' ? 'Permanently remove synthetic-data and its data.' : 'Original data remains intact until validated publication.',
      })
    }
    if (path === '/api/data/operations' && method === 'POST') {
      const request = route.request().postDataJSON() as DataRequest
      requests.push(request)
      const operation: DataOperation = {
        id: 12, request, status: 'running', phase: 'queued', bytes: 0, startedAt: '2026-01-01T00:00:00Z', result: {},
      }
      operations = [operation]
      return json(operation, 202)
    }
    if (path === '/api/data/operations/12' && method === 'DELETE') {
      operations = operations.map((run) => ({ ...run, status: 'cancelled', phase: 'cancelled' }))
      return json({ status: 'cancellation-requested' })
    }
    if (path === '/api/docker/backups' && method === 'POST') {
      const saved = route.request().postDataJSON() as BackupSchedule
      saves.push(saved)
      currentSchedules = [{ ...saved, id: saved.id || 1, nextRunAt: schedule.nextRunAt }]
      return json(currentSchedules[0])
    }
    if (path.startsWith('/api/docker/backups/') && method === 'DELETE') {
      deleted.push(path)
      currentSchedules = []
      return route.fulfill({ status: 204 })
    }
    const responses: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false },
      '/api/docker/status': { available: true, enabled: true, context: 'porto' },
      '/api/docker/cleanup': { runs: [], enabled: false, dockerEnabled: true },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic', revision: 1, available: true, stale: false, containers: [], capabilities: {} },
      '/api/docker/storage': currentUsage,
      '/api/docker/volumes': [{ name: resource.name, driver: 'local', mountpoint: '/synthetic/volume/_data', scope: 'local' }],
      '/api/docker/volumes/synthetic-data/storage': volume,
      '/api/docker/volumes/synthetic-data/files': { resource, identity: volume.identity, path: '.', entries: [], readOnly: false, truncated: false },
      '/api/docker/backups': currentSchedules,
      '/api/data/operations': operations,
      '/api/files/attachments': [],
    }
    return json(responses[path] ?? [])
  })
  return {
    requests, saves, deleted,
    setOperations: (rows: DataOperation[]) => { operations = rows },
    setUsage: (value: StorageUsage) => { currentUsage = value },
  }
}

for (const theme of ['dark', 'light']) {
  for (const width of [1440, 1024, 390]) {
    test(`Storage Center layout at ${width}px in ${theme} theme`, async ({ page }, testInfo) => {
      await page.setViewportSize({ width, height: 960 })
      await page.addInitScript((theme) => {
        localStorage.setItem('porto.experiencePreferences.v1', JSON.stringify({ theme, reduceMotion: true }))
      }, theme)
      const fixture = await mockStorage(page, [], visualUsage)
      await page.route('**/api/settings', (route) => route.fulfill({
        json: { dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false, theme, reduceMotion: true },
      }))
      fixture.setOperations([{
        id: 21, request: { action: 'volume-export', resource },
        status: 'running', phase: 'archiving', bytes: 1024 ** 3,
        startedAt: '2026-01-01T00:00:00Z', result: {},
      }])
      await page.goto('/#/docker-storage')
      await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
      await expect(page.getByRole('button', { name: 'Cancel operation 21' })).toBeVisible()
      await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
      await page.screenshot({ path: testInfo.outputPath(`storage-${theme}-${width}-closed.png`), fullPage: true })
      const noPageOverflow = await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
      const manage = page.getByRole('button', { name: 'Manage volume synthetic-data' })
      await manage.click()
      const inspector = page.getByRole('complementary', { name: 'synthetic-data inspector' })
      await expect(inspector.getByRole('heading', { name: 'synthetic-data', exact: true })).toBeFocused()
      await expect(inspector.getByLabel('Volume action')).toBeVisible()
      await page.screenshot({ path: testInfo.outputPath(`storage-${theme}-${width}-open.png`), fullPage: true })
      expect(noPageOverflow).toBe(true)
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
      if (width > 860) {
        const inventory = await page.getByRole('table', { name: 'Storage resources', exact: true }).boundingBox()
        const detail = await inspector.boundingBox()
        expect(inventory).not.toBeNull()
        expect(detail).not.toBeNull()
        const scroll = page.getByRole('region', { name: 'Scrollable storage resources' })
        const bounds = await scroll.boundingBox()
        expect(bounds).not.toBeNull()
        expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(detail!.x)
      }
      await page.keyboard.press('Escape')
      await expect(inspector).not.toBeVisible()
      await expect(manage).toBeFocused()
      const scroll = page.getByRole('region', { name: 'Scrollable storage resources' })
      await scroll.focus()
      await expect(scroll).toBeFocused()
      if (width <= 1024) {
        expect(await scroll.evaluate((element) => element.scrollWidth > element.clientWidth)).toBe(true)
        await page.keyboard.press('ArrowRight')
        await expect.poll(() => scroll.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0)
      }
      if (width === 1440) {
        const controls = [
          page.getByLabel('Search storage resources and owners'),
          page.getByLabel('Resource type', { exact: true }),
          page.getByLabel('Reference filter', { exact: true }),
          page.getByRole('button', { name: 'Refresh storage' }),
        ]
        const bottoms = await Promise.all(controls.map(async (control) => {
          const box = await control.boundingBox()
          return box!.y + box!.height
        }))
        expect(Math.max(...bottoms) - Math.min(...bottoms)).toBeLessThanOrEqual(1)
      }
    })
  }
}

test('storage labels distinguish measurement, protection and cleanup scope', async ({ page }) => {
  const fixture = await mockStorage(page, [], visualUsage)
  await page.goto('/#/docker-storage')
  const table = page.getByRole('table', { name: 'Storage resources', exact: true })
  for (const name of ['Logical', 'Allocated', 'Shared']) {
    await expect(table.getByRole('columnheader', { name, exact: true })).toBeVisible()
  }
  await expect(page.getByText('Writable snapshots are included in snapshots.')).toBeVisible()
  await expect(page.getByRole('checkbox', { name: 'Select volume synthetic-data' })).toHaveAccessibleDescription(/In use/)
  const protectedResource = page.getByRole('row').filter({ hasText: 'Managed Kubernetes content remains protected' })
  await expect(protectedResource).toContainText('Protected')
  await expect(protectedResource.getByRole('checkbox')).toBeDisabled()
  await expect(page.getByText('4 of 4 resources')).toBeVisible()
  const selection = page.getByRole('group', { name: 'Selected resources', exact: true })
  await expect(selection).toContainText('0 selected')
  await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
  await expect(selection).toContainText('1 selected')
  await page.getByRole('button', { name: 'Clear selection', exact: true }).click()
  await expect(selection).toContainText('0 selected')
  await expect(page.getByRole('button', { name: 'Preview selected cleanup' })).toBeDisabled()
  await page.getByLabel('Cleanup category').selectOption('cache')
  await page.getByLabel('Search storage resources and owners').fill('synthetic-data')
  await expect(page.getByText('1 of 4 resources')).toBeVisible()
  await expect(page.getByRole('group', { name: 'Category cleanup', exact: true })).toContainText('ignores the resource filters and selection')
  await page.getByRole('button', { name: 'Preview category cleanup' }).click()
  expect(fixture.requests[0]).toMatchObject({ action: 'prune', categories: ['cache'] })
  await expect(page.getByRole('region', { name: 'Scoped cleanup preview' })).toContainText('Category: Build cache')
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Confirm exact cleanup' }).click()
  expect(fixture.requests).toHaveLength(1)
})

test('storage filters reset without retaining a hidden cleanup selection', async ({ page }) => {
  await mockStorage(page)
  await page.goto('/#/docker-storage')
  await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
  await page.getByLabel('Resource type', { exact: true }).selectOption('volume')
  await expect(page.getByRole('group', { name: 'Selected resources', exact: true })).toContainText('0 selected')
  await page.getByLabel('Search storage resources and owners').fill('synthetic-no-match')
  await expect(page.getByText('No resources match these filters.')).toBeVisible()
  await page.getByRole('button', { name: 'Clear filters', exact: true }).click()
  await expect(page.getByLabel('Search storage resources and owners')).toHaveValue('')
  await expect(page.getByLabel('Resource type', { exact: true })).toHaveValue('all')
  await expect(page.getByText('2 of 2 resources')).toBeVisible()
})

test('storage ownership changes still block exact cleanup confirmation', async ({ page }) => {
  await page.clock.install()
  const fixture = await mockStorage(page)
  await page.goto('/#/docker-storage')
  await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
  await page.getByRole('button', { name: 'Preview selected cleanup' }).click()
  await expect(page.getByRole('button', { name: 'Confirm exact cleanup' })).toBeEnabled()
  fixture.setUsage({ ...usage, resources: [volume, { ...cache, protected: true, reason: 'Synthetic active build' }] })
  await page.clock.fastForward(30000)
  await expect(page.getByText('Resource ownership changed or could not be refreshed. Preview again before cleanup.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Confirm exact cleanup' })).toBeDisabled()
  await expect(page.getByRole('group', { name: 'Selected resources', exact: true })).toContainText('0 selected')
  expect(fixture.requests).toHaveLength(1)
})

test('storage loading, empty and failed measurements are distinct', async ({ page }) => {
  await mockStorage(page)
  let release: (() => void) | undefined
  const pending = new Promise<void>((resolve) => { release = resolve })
  await page.route('**/api/docker/storage', async (route) => {
    await pending
    await route.fulfill({ json: { ...usage, resources: [] } })
  })
  await page.goto('/#/docker-storage')
  await expect(page.getByRole('status').filter({ hasText: 'Measuring stores and resource ownership' })).toBeVisible()
  await expect(page.getByText('No storage resources reported.')).not.toBeVisible()
  await expect(page.getByText('No resources match these filters.')).not.toBeVisible()
  release?.()
  await expect(page.getByText('No storage resources reported.')).toBeVisible()
  await page.route('**/api/docker/storage', (route) => route.fulfill({ status: 503, json: { message: 'Synthetic storage measurement unavailable' } }))
  await page.getByRole('button', { name: 'Refresh storage' }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic storage measurement unavailable' })).toBeVisible()
  await expect(page.getByText('No storage resources reported.')).not.toBeVisible()
  await expect(page.getByRole('button', { name: 'Preview category cleanup' })).toBeDisabled()
})

for (const cancelWith of ['cancel button', 'resource filter']) {
  test(`pending cleanup preview is invalidated by ${cancelWith}`, async ({ page }) => {
    const fixture = await mockStorage(page)
    let release: (() => void) | undefined
    const pending = new Promise<void>((resolve) => { release = resolve })
    await page.route('**/api/docker/storage/preview', async (route) => {
      await pending
      await route.fallback()
    })
    await page.goto('/#/docker-storage')
    await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
    await page.getByRole('button', { name: 'Preview selected cleanup' }).click()
    await expect(page.getByRole('status').filter({ hasText: 'Preparing exact cleanup preview' })).toBeVisible()
    if (cancelWith === 'cancel button') await page.getByRole('button', { name: 'Cancel preparation' }).click()
    else await page.getByLabel('Resource type', { exact: true }).selectOption('volume')
    release?.()
    await expect.poll(() => fixture.requests.length).toBe(1)
    await expect(page.getByRole('region', { name: 'Scoped cleanup preview' })).not.toBeVisible()
    await expect(page.getByRole('button', { name: 'Cancel preparation' })).not.toBeVisible()
    await expect(page.getByRole('group', { name: 'Selected resources', exact: true })).toContainText(cancelWith === 'cancel button' ? '1 selected' : '0 selected')
  })
}

test('cleanup start is not presented as cancellable preparation or guaranteed reclaimed space', async ({ page }) => {
  await page.clock.install()
  const fixture = await mockStorage(page)
  let release: (() => void) | undefined
  const pending = new Promise<void>((resolve) => { release = resolve })
  await page.route('**/api/data/operations', async (route) => {
    if (route.request().method() === 'POST') await pending
    await route.fallback()
  })
  await page.goto('/#/docker-storage')
  await page.getByRole('checkbox', { name: 'Select cache synthetic-cache' }).check()
  await page.getByRole('button', { name: 'Preview selected cleanup' }).click()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Confirm exact cleanup' }).click()
  await expect(page.getByRole('status').filter({ hasText: 'Starting cleanup.' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Cancel preparation' })).not.toBeVisible()
  release?.()
  await page.clock.fastForward(2000)
  await expect(page.getByRole('button', { name: 'Cancel operation 12' })).toBeVisible()
  await expect(page.getByText('Actual reclaimed bytes:', { exact: false })).not.toBeVisible()
  fixture.setOperations([{
    id: 12, request: { action: 'prune' }, status: 'succeeded', phase: 'completed',
    bytes: 0, startedAt: '2026-01-01T00:00:00Z', result: {},
  }])
  await page.clock.fastForward(2000)
  await expect(page.getByText('Actual reclaimed bytes: Unavailable')).toBeVisible()
})

test('empty cleanup previews and preparation errors remain actionable', async ({ page }) => {
  await mockStorage(page)
  await page.route('**/api/docker/storage/preview', (route) => route.fulfill({
    status: 409, json: { message: 'Synthetic ownership changed. Refresh storage and preview again.' },
  }))
  await page.goto('/#/docker-storage')
  await page.getByLabel('Cleanup category').selectOption('volume')
  await page.getByRole('button', { name: 'Preview category cleanup' }).click()
  await expect(page.getByRole('region', { name: 'Scoped cleanup controls' }).getByRole('alert')).toContainText('Synthetic ownership changed.')
  await expect(page.getByRole('button', { name: 'Preview category cleanup' })).toBeEnabled()
  await page.route('**/api/docker/storage/preview', (route) => route.fulfill({
    json: {
      request: { action: 'prune', selections: [] }, candidates: [], excluded: [volume],
      upperBoundBytes: 0, token: 'synthetic-empty-preview', message: 'All matching resources are protected or referenced.',
    },
  }))
  await page.getByRole('button', { name: 'Preview category cleanup' }).click()
  await expect(page.getByText('No eligible unused resources in this scope. Nothing will be removed.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Confirm exact cleanup' })).toBeDisabled()
  await expect(page.getByRole('region', { name: 'Scoped cleanup controls' }).getByRole('alert')).not.toBeVisible()
})

test('volume inspector returns focus to search if its resource is filtered out', async ({ page }) => {
  await mockStorage(page)
  await page.goto('/#/docker-storage')
  await page.getByRole('button', { name: 'Manage volume synthetic-data' }).click()
  const search = page.getByLabel('Search storage resources and owners')
  await search.fill('synthetic-no-match')
  await expect(page.getByRole('button', { name: 'Manage volume synthetic-data' })).not.toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('complementary', { name: 'synthetic-data inspector' })).not.toBeVisible()
  await expect(search).toBeFocused()
})

test('storage cleanup is category-scoped, identity-bound and cancellable', async ({ page }, testInfo) => {
  const fixture = await mockStorage(page)
  await page.goto('/#/volumes')
  const storageLink = page.locator('#rail-group-containers').getByRole('link', { name: 'Storage tools', exact: true })
  await expect(storageLink).toHaveAttribute('href', '#/docker-storage')
  await storageLink.click()
  const category = page.getByLabel('Cleanup category')
  await expect(category).toHaveValue('')
  await expect(page.getByRole('button', { name: 'Preview category cleanup' })).toBeDisabled()
  await expect(page.getByRole('checkbox', { name: 'Select volume synthetic-data' })).toBeDisabled()
  await expect(page.getByText('Synthetic build copy')).toBeVisible()
  await expect(page.getByText(/synthetic-stack\/database/).first()).toBeVisible()
  await category.selectOption('cache')
  await page.getByRole('button', { name: 'Preview category cleanup' }).click()
  await expect(page.getByRole('region', { name: 'Scoped cleanup preview' })).toBeVisible()
  expect(fixture.requests[0]).toMatchObject({ action: 'prune', categories: ['cache'] })
  await expect(page.getByText(/synthetic-data.*excluded|excluded.*synthetic-data/)).toBeVisible()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Confirm exact cleanup' }).click()
  await expect.poll(() => fixture.requests.length).toBe(2)
  expect(fixture.requests[1]).toMatchObject({
    action: 'prune', confirm: true, preview: 'synthetic-prune',
    selections: [{ kind: 'cache', name: 'synthetic-cache', id: 'synthetic-cache' }],
  })
  expect(fixture.requests[1].categories).toBeUndefined()
  await expect(page.getByRole('button', { name: 'Cancel operation 12' })).toBeVisible()
  await page.getByRole('button', { name: 'Cancel operation 12' }).click()
  await expect(page.getByText(/cancelled.*cancelled/)).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('storage-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.reload()
  await expect(page.getByLabel('Cleanup category')).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('storage-mobile.png'), fullPage: true })
})

test('volume management shows mount owners and edits backup retention', async ({ page }, testInfo) => {
  const fixture = await mockStorage(page, [schedule])
  await page.goto('/#/volumes')
  await page.getByText('synthetic-data', { exact: true }).first().click()
  await expect(page.getByText('/data', { exact: true })).toBeVisible()
  await expect(page.getByText(/synthetic-stack\/database/)).toBeVisible()
  await page.getByRole('tab', { name: 'Data and backups' }).click()
  await page.getByRole('button', { name: 'Edit backup policy' }).click()
  await page.getByLabel('Verified archives to retain').fill('3')
  await page.getByLabel('Interval in hours').fill('12')
  await page.getByRole('button', { name: 'Save backup policy' }).click()
  await expect.poll(() => fixture.saves.length).toBe(1)
  expect(fixture.saves[0]).toMatchObject({ id: 1, intervalHours: 12, retention: 3, enabled: true, resource })
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Remove backup schedule' }).click()
  await expect.poll(() => fixture.deleted.length).toBe(1)
  await expect(page.getByRole('button', { name: 'Enable local backups' })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('volume-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.reload()
  await page.getByText('synthetic-data', { exact: true }).first().click()
  await page.getByRole('tab', { name: 'Data and backups' }).click()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('volume-mobile.png'), fullPage: true })
})

test('volume removal opens a preview rather than an untracked direct delete', async ({ page }) => {
  const fixture = await mockStorage(page)
  const directDeletes: string[] = []
  page.on('request', (request) => {
    if (request.method() === 'DELETE' && request.url().includes('/api/docker/volumes/')) directDeletes.push(request.url())
  })
  await page.goto('/#/volumes')
  await page.getByRole('button', { name: 'Remove volume', exact: true }).first().click()
  await expect(page.getByLabel('Volume action')).toHaveValue('volume-remove')
  await page.getByRole('button', { name: 'Preview volume action' }).click()
  await expect(page.getByRole('region', { name: 'Volume action preview' })).toContainText('synthetic-data')
  expect(directDeletes).toEqual([])
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Confirm previewed volume action' }).click()
  await expect.poll(() => fixture.requests.length).toBe(2)
  expect(fixture.requests[1]).toMatchObject({ action: 'volume-remove', identity: volume.identity, confirm: true })
})

test('local archive import is available before any volume exists', async ({ page }) => {
  await mockStorage(page)
  await page.route('**/api/docker/volumes', (route) => route.fulfill({ contentType: 'application/json', body: '[]' }))
  await page.goto('/#/volumes')
  await page.getByRole('button', { name: 'Import local archive' }).click()
  await expect(page.getByLabel('Volume action')).toHaveValue('volume-import')
  await expect(page.getByLabel('New destination volume')).toBeVisible()
  await expect(page.getByLabel('Versioned volume archive')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Preview volume action' })).toBeDisabled()
})
