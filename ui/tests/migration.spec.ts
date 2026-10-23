import { expect, test, type Page } from '@playwright/test'
import type { DataOperation, DataRequest } from '../src/dataTypes'

async function mockMigration(page: Page, operations: DataOperation[] = []) {
  page.on('pageerror', (error) => { throw error })
  const requests: DataRequest[] = []
  await page.route('**/api/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    if (path === '/api/docker/storage/preview') {
      const request = route.request().postDataJSON()
      requests.push(request)
      await route.fulfill({
        contentType: 'application/json',
        body: JSON.stringify({
          request: { ...request, preview: 'synthetic-preview-token' },
          objects: [{ kind: 'volume', name: 'fixture-unattached', id: 'fixture-volume' }],
          conflicts: request.allowSourceHelper ? [] : ['fixture-unattached: explicit source helper consent required'],
          warnings: [], token: 'synthetic-preview-token', sourcePolicy: 'Original source resources remain unchanged; helpers are never started.',
          sourceHelpers: ['fixture-unattached'],
        }),
      })
      return
    }
    if (path === '/api/data/operations' && route.request().method() === 'POST') {
      requests.push(route.request().postDataJSON())
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ id: 1, status: 'running' }) })
      return
    }
    const responses: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: true },
      '/api/docker/cleanup': { runs: [], enabled: false },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic-fixture', revision: 1, available: true, stale: false, containers: [], capabilities: {} },
      '/api/files/attachments': [],
      '/api/data/operations': operations,
      '/api/docker/migration/contexts': [
        { name: 'fixture-context', endpoint: 'unix:///fixture.sock', supported: true, desktop: true },
        { name: 'fixture-second', endpoint: 'unix:///second.sock', supported: true, desktop: false },
      ],
      '/api/docker/migration/inventory': {
        context: { name: new URL(route.request().url()).searchParams.get('context'), supported: true },
        objects: [{ kind: 'volume', name: 'fixture-unattached', id: 'fixture-volume', supported: true }],
        warnings: [],
      },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(responses[path] ?? []) })
  })
  return requests
}

test('unattached volume helper access is off by default and consent survives confirmation', async ({ page }) => {
  const requests = await mockMigration(page)
  await page.goto('/#/migration')
  await page.getByLabel('Source Docker context').selectOption('fixture-context')
  await page.getByRole('checkbox', { name: 'Migrate volume fixture-unattached' }).check()
  const consent = page.getByRole('checkbox', { name: /Allow temporary read-only source access/ })
  await expect(consent).not.toBeChecked()
  await page.getByRole('button', { name: 'Dry-run selected migration' }).click()
  await expect(page.getByRole('button', { name: 'Confirm selected migration' })).toBeDisabled()
  expect(requests[0].allowSourceHelper).toBe(false)
  await consent.check()
  await page.getByRole('button', { name: 'Dry-run selected migration' }).click()
  await expect(page.getByRole('button', { name: 'Confirm selected migration' })).toBeEnabled()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Confirm selected migration' }).click()
  await expect.poll(() => requests.length).toBe(3)
  expect(requests[2]).toMatchObject({ action: 'migration', allowSourceHelper: true, confirm: true })
})

function interruptedMigration(id = 'fixture-volume'): DataOperation {
  return {
    id: 7, status: 'interrupted', phase: 'interrupted', bytes: 1024, startedAt: '2026-01-01T00:00:00Z',
    request: {
      action: 'migration', context: 'fixture-context',
      selections: [{ kind: 'volume', name: 'fixture-unattached', id, destination: 'fixture-restored' }],
      includeSensitive: true, allowSourceHelper: true, confirm: true, preview: 'expired-preview',
    },
    result: { steps: [{ kind: 'volume', source: 'fixture-unattached', destination: 'fixture-restored', status: 'interrupted' }] },
  }
}

test('interrupted migrations require a fresh dry run and fresh sensitive/helper consent', async ({ page }, testInfo) => {
  const requests = await mockMigration(page, [interruptedMigration()])
  await page.goto('/#/migration')
  await page.getByRole('button', { name: 'Review migration 7' }).click()
  await expect(page.getByLabel('Source Docker context')).toHaveValue('fixture-context')
  await expect(page.getByRole('checkbox', { name: 'Migrate volume fixture-unattached' })).toBeChecked()
  await expect(page.getByLabel('Destination for fixture-unattached')).toHaveValue('fixture-restored')
  await expect(page.getByRole('checkbox', { name: /Explicitly allow sensitive/ })).not.toBeChecked()
  await expect(page.getByRole('checkbox', { name: /Allow temporary read-only source access/ })).not.toBeChecked()
  await expect(page.getByRole('button', { name: 'Confirm selected migration' })).toHaveCount(0)
  await page.getByRole('button', { name: 'Dry-run selected migration' }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0]).toMatchObject({
    action: 'migration', context: 'fixture-context', includeSensitive: false, allowSourceHelper: false,
    selections: [{ kind: 'volume', name: 'fixture-unattached', id: 'fixture-volume', destination: 'fixture-restored' }],
  })
  expect(requests[0].preview).toBeUndefined()
  expect(requests[0].confirm).toBeUndefined()
  await page.screenshot({ path: testInfo.outputPath('migration-recovery-desktop.png'), fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.reload()
  await expect(page.getByRole('button', { name: 'Review migration 7' })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('migration-recovery-mobile.png'), fullPage: true, animations: 'disabled' })
})

test('migration retries never silently retarget recreated source names', async ({ page }) => {
  await mockMigration(page, [interruptedMigration('fixture-old-volume')])
  await page.goto('/#/migration')
  await page.getByRole('button', { name: 'Review migration 7' }).click()
  await expect(page.getByRole('checkbox', { name: 'Migrate volume fixture-unattached' })).not.toBeChecked()
  await expect(page.getByRole('alert').filter({ hasText: /selected source identit.*no longer/ })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Dry-run selected migration' })).toBeDisabled()
  await page.getByRole('button', { name: 'Clear unavailable selections' }).click()
  await page.getByRole('checkbox', { name: 'Migrate volume fixture-unattached' }).check()
  await expect(page.getByRole('button', { name: 'Dry-run selected migration' })).toBeEnabled()
})

test('changing the migration source clears sensitive transfer consent', async ({ page }) => {
  await mockMigration(page)
  await page.goto('/#/migration')
  await page.getByLabel('Source Docker context').selectOption('fixture-context')
  const consent = page.getByRole('checkbox', { name: /Explicitly allow sensitive/ })
  await consent.check()
  await page.getByLabel('Source Docker context').selectOption('fixture-second')
  await expect(consent).not.toBeChecked()
})
