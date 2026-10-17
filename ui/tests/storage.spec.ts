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
const schedule: BackupSchedule = {
  id: 1, resource, enabled: true, intervalHours: 24, retention: 7,
  directory: '/synthetic/backups/data', nextRunAt: '2026-01-02T00:00:00Z',
}

async function mockStorage(page: Page, schedules: BackupSchedule[] = []) {
  const requests: DataRequest[] = []
  const saves: BackupSchedule[] = []
  const deleted: string[] = []
  let operations: DataOperation[] = []
  let currentSchedules = schedules
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
      '/api/docker/storage': usage,
      '/api/docker/volumes': [{ name: resource.name, driver: 'local', mountpoint: '/synthetic/volume/_data', scope: 'local' }],
      '/api/docker/volumes/synthetic-data/storage': volume,
      '/api/docker/volumes/synthetic-data/files': { resource, identity: volume.identity, path: '.', entries: [], readOnly: false, truncated: false },
      '/api/docker/backups': currentSchedules,
      '/api/data/operations': operations,
      '/api/files/attachments': [],
    }
    return json(responses[path] ?? [])
  })
  return { requests, saves, deleted, setOperations: (rows: DataOperation[]) => { operations = rows } }
}

test('storage cleanup is category-scoped, identity-bound and cancellable', async ({ page }, testInfo) => {
  const fixture = await mockStorage(page)
  await page.goto('/#/volumes')
  const storageLink = page.locator('#rail-group-containers').getByRole('link', { name: 'Storage', exact: true })
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
