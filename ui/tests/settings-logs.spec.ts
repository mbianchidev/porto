import { expect, test, type Page } from '@playwright/test'
import type { Settings } from '../src/types'

const settings: Settings = {
  cleanupLocalMerged: false, cleanupRemoteMerged: false, pruneRemoteTracking: false,
  protectedBranches: ['main'], sqlNotSoLiteEnabled: false, killSwitchEnabled: false,
  sendboxEnabled: false, dockerEnabled: false, dockerAutoPruneEnabled: false,
  kubernetesEnabled: false, vmsEnabled: false, interfaceDensity: 'compact', theme: 'dark',
  reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2,
  terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7,
  logMaxFileMB: 10, logMaxTotalMB: 50,
}

async function mockSettings(page: Page, current: Settings) {
  const saved: Settings[] = []
  const integration = { state: 'disabled', message: 'Synthetic integration disabled.', updatedAt: '2026-01-01T00:00:00Z' }
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    if (path === '/api/settings' && request.method() === 'PUT') {
      const body = request.postDataJSON() as Settings
      saved.push(body)
      await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ ...current, ...body }) })
      return
    }
    const values: Record<string, unknown> = {
      '/api/settings': current,
      '/api/health': { version: '1.2.3' },
      '/api/integrations/sql-not-so-lite': integration,
      '/api/integrations/sendbox': integration,
      '/api/integrations/kill-switch': { ...integration, supported: false, installed: false, autoKillEnabled: null, userPorts: [], syncedPorts: [], effectivePorts: [] },
      '/api/docker/status': { enabled: false, available: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(values[path] ?? []) })
  })
  return saved
}

test('log size limits are editable in MB and validated before saving', async ({ page }) => {
  const saved = await mockSettings(page, settings)
  await page.goto('/#/settings')
  const logs = page.getByRole('region', { name: 'Keep logs bounded.' })
  const file = logs.getByLabel('Rotate the log file at (MB)')
  const total = logs.getByLabel('Keep all diagnostic logs under (MB)')
  const save = logs.getByRole('button', { name: 'Save log settings', exact: true })
  await expect(file).toHaveValue('10')
  await expect(total).toHaveValue('50')

  await file.fill('60')
  await expect(logs.getByRole('alert')).toHaveText('The total limit must be larger than the file limit to leave room for archives.')
  await expect(save).toBeDisabled()

  await total.fill('200')
  await expect(save).toBeEnabled()
  await save.click()
  await expect.poll(() => saved.length).toBe(1)
  expect(saved[0]).toMatchObject({ logMaxFileMB: 60, logMaxTotalMB: 200, logRetentionDays: 7 })

  await logs.getByRole('button', { name: 'Reset to 7 days, 10 MB and 50 MB', exact: true }).click()
  await expect(file).toHaveValue('10')
  await expect(total).toHaveValue('50')
})

test('environment log size limits are shown as read-only', async ({ page }) => {
  await mockSettings(page, { ...settings, logMaxFileMB: 5, logMaxTotalMB: 80, logSizeLimitsFromEnvironment: true })
  await page.goto('/#/settings')
  const logs = page.getByRole('region', { name: 'Keep logs bounded.' })
  await expect(logs.getByLabel('Rotate the log file at (MB)')).toBeDisabled()
  await expect(logs.getByLabel('Keep all diagnostic logs under (MB)')).toBeDisabled()
  await expect(logs.getByText(/PORTO_LOG_MAX_FILE_MB or PORTO_LOG_MAX_TOTAL_MB sets these limits/)).toBeVisible()
  await expect(logs.getByRole('button', { name: 'Save log settings', exact: true })).toBeEnabled()
})
