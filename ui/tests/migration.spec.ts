import { expect, test } from '@playwright/test'

test('unattached volume helper access is off by default and consent survives confirmation', async ({ page }) => {
  page.on('pageerror', (error) => { throw error })
  const requests: Array<{ action?: string; allowSourceHelper?: boolean; confirm?: boolean }> = []
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
      '/api/data/operations': [],
      '/api/docker/migration/contexts': [{ name: 'fixture-context', endpoint: 'unix:///fixture.sock', supported: true, desktop: true }],
      '/api/docker/migration/inventory': {
        context: { name: 'fixture-context', supported: true },
        objects: [{ kind: 'volume', name: 'fixture-unattached', id: 'fixture-volume', supported: true }],
        warnings: [],
      },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(responses[path] ?? []) })
  })
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
