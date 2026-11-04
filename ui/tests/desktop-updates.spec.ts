import { expect, test, type Page } from '@playwright/test'
import type { DesktopUpdatePhase, DesktopUpdateStatus, IntegrationStatus, KillSwitchStatus, Settings } from '../src/types'

const settings: Settings = {
  cleanupLocalMerged: false, cleanupRemoteMerged: false, pruneRemoteTracking: false,
  protectedBranches: ['main'], sqlNotSoLiteEnabled: false, killSwitchEnabled: false,
  sendboxEnabled: false, dockerEnabled: false, dockerAutoPruneEnabled: false,
  kubernetesEnabled: false, vmsEnabled: false, interfaceDensity: 'compact', theme: 'dark',
  reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2,
  terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7,
}

declare global {
  interface Window {
    fixtureDesktopUpdates?: {
      checks: number
      downloads: number
      installs: number
      statusError: string
      complete: (status: DesktopUpdateStatus) => void
      fail: (message: string) => void
      reject: (message: string) => void
      publish: (status: DesktopUpdateStatus) => void
    }
  }
}

function updateStatus(phase: DesktopUpdatePhase, message = 'Synthetic update status.'): DesktopUpdateStatus {
  return {
    phase, currentVersion: '1.2.3', availableVersion: '', progressPercent: null,
    releaseURL: '', message,
  }
}

async function mockSettings(page: Page) {
  const integration = {
    state: 'disabled', message: 'Synthetic integration disabled.', updatedAt: '2026-01-01T00:00:00Z',
  } satisfies IntegrationStatus
  const killSwitch: KillSwitchStatus = {
    ...integration, supported: false, installed: false, autoKillEnabled: null,
    userPorts: [], syncedPorts: [], effectivePorts: [],
  }
  await page.route('**/api/**', async (route) => {
    const values: Record<string, unknown> = {
      '/api/settings': settings,
      '/api/health': { version: '1.2.3' },
      '/api/integrations/sql-not-so-lite': integration,
      '/api/integrations/sendbox': integration,
      '/api/integrations/kill-switch': killSwitch,
      '/api/docker/status': { enabled: false, available: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
    }
    await route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify(values[new URL(route.request().url()).pathname] ?? []),
    })
  })
}

async function mockDesktop(
  page: Page,
  phase: DesktopUpdatePhase = 'idle',
  options: { preferencesError?: string; statusError?: string } = {},
) {
  await page.addInitScript(({ initialStatus, preferencesError, statusError }: {
    initialStatus: DesktopUpdateStatus
    preferencesError: string
    statusError: string
  }) => {
    let status = initialStatus
    const listeners = new Set<(status: DesktopUpdateStatus) => void>()
    let resolveCheck: ((status: DesktopUpdateStatus) => void) | null = null
    let rejectCheck: ((error: Error) => void) | null = null
    const fixture = {
      checks: 0,
      downloads: 0,
      installs: 0,
      statusError,
      publish(next: DesktopUpdateStatus) {
        status = next
        for (const listener of listeners) listener(status)
      },
      complete(next: DesktopUpdateStatus) {
        fixture.publish(next)
        resolveCheck?.(next)
      },
      fail(message: string) {
        fixture.publish({ ...status, phase: 'error', message })
        rejectCheck?.(new Error(message))
      },
      reject(message: string) {
        rejectCheck?.(new Error(message))
      },
    }
    window.fixtureDesktopUpdates = fixture
    window.portoDesktop = {
      openFiles: async () => {},
      getPreferences: async () => {
        if (preferencesError) throw new Error(preferencesError)
        return {
          openAtLogin: false, keepInTray: false, automaticallyDownloadUpdates: false,
          loginItemSupported: true,
        }
      },
      setPreferences: async (preferences) => ({ ...preferences, loginItemSupported: true }),
      getUpdateStatus: async () => {
        if (fixture.statusError) throw new Error(fixture.statusError)
        return status
      },
      onUpdateStatus(listener) {
        listeners.add(listener)
        return () => { listeners.delete(listener) }
      },
      checkForUpdates() {
        fixture.checks++
        fixture.publish({ ...status, phase: 'checking', message: 'Checking synthetic releases.' })
        return new Promise<DesktopUpdateStatus>((resolve, reject) => {
          resolveCheck = resolve
          rejectCheck = reject
        })
      },
      downloadUpdate: async () => {
        fixture.downloads++
        const next = { ...status, phase: 'downloaded' as const, message: 'Synthetic update is ready to install.' }
        fixture.publish(next)
        return next
      },
      restartAndInstallUpdate: async () => {
        fixture.installs++
        const next = { ...status, phase: 'installing' as const, message: 'Installing synthetic update.' }
        fixture.publish(next)
        return next
      },
    }
  }, {
    initialStatus: updateStatus(phase, phase === 'unsupported' ? 'Update checks are available in installed Porto builds.' : undefined),
    preferencesError: options.preferencesError ?? '',
    statusError: options.statusError ?? '',
  })
}

test('software updates are visible before appearance settings without scrolling', async ({ page }, testInfo) => {
  await page.setViewportSize({ width: 1280, height: 900 })
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  await page.screenshot({ path: testInfo.outputPath('settings.png'), animations: 'disabled' })
  const updates = page.getByRole('region', { name: 'Software updates', exact: true })
  await expect(updates).toBeInViewport()
  await expect(updates.getByRole('button', { name: 'Check for updates', exact: true })).toBeInViewport()
  const appearance = page.getByRole('region', { name: 'Tune the control desk to your working rhythm.' })
  const updatesBox = await updates.boundingBox()
  const appearanceBox = await appearance.boundingBox()
  if (!updatesBox || !appearanceBox) throw new Error('Expected visible update and appearance sections')
  expect(updatesBox.y + updatesBox.height).toBeLessThan(appearanceBox.y)
})

test('source desktop builds do not offer an unsupported update check', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page, 'unsupported')
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeDisabled()
  await expect(page.getByText('Update checks are available in installed Porto builds.')).toBeVisible()
})

test('browser dashboards explain why desktop updates are unavailable', async ({ page }) => {
  await mockSettings(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeDisabled()
  await expect(page.getByText('Open Porto as the installed desktop app to check for updates.')).toBeVisible()
})

test('manual Settings checks show progress and a fresh result', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  await page.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await expect(page.getByRole('button', { name: 'Checking...', exact: true })).toBeDisabled()
  await expect(page.getByText('Checking synthetic releases.')).toBeVisible()
  await page.evaluate((status) => window.fixtureDesktopUpdates?.complete(status),
    updateStatus('up-to-date', 'Porto 1.2.3 is up to date.'))
  await expect(page.getByText('Porto 1.2.3 is up to date.')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeEnabled()
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.checks)).toBe(1)
})

test('failed manual Settings checks report the error and allow retry', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  await page.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate(() => window.fixtureDesktopUpdates?.fail('Synthetic release service unavailable.'))
  await expect(page.getByRole('region', { name: 'Software updates', exact: true }).getByRole('alert')).toContainText('Synthetic release service unavailable.')
  await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate((status) => window.fixtureDesktopUpdates?.complete(status),
    updateStatus('up-to-date', 'Porto 1.2.3 is up to date.'))
  await expect(page.getByText('Porto 1.2.3 is up to date.')).toBeVisible()
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.checks)).toBe(2)
})

test('update failures stay visible even when the desktop bridge does not publish a status', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  const updates = page.getByRole('region', { name: 'Software updates', exact: true })
  await updates.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate(() => window.fixtureDesktopUpdates?.reject('Synthetic IPC update failure.'))
  await expect(updates.getByRole('alert')).toContainText('Synthetic IPC update failure.')
  await expect(updates.getByRole('button', { name: 'Check for updates', exact: true })).toBeEnabled()
})

test('update checks remain available when desktop preferences cannot be loaded', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page, 'idle', { preferencesError: 'Synthetic desktop preferences unavailable.' })
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  const updates = page.getByRole('region', { name: 'Software updates', exact: true })
  await updates.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate((status) => window.fixtureDesktopUpdates?.complete(status),
    updateStatus('up-to-date', 'Porto 1.2.3 is up to date.'))
  await expect(updates.getByRole('status')).toContainText('Porto 1.2.3 is up to date.')
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.checks)).toBe(1)
})

test('unavailable update status has an explicit retry before checks can run', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page, 'idle', { statusError: 'Synthetic update status unavailable.' })
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  const updates = page.getByRole('region', { name: 'Software updates', exact: true })
  await expect(updates.getByRole('alert')).toContainText('Synthetic update status unavailable.')
  await expect(updates.getByRole('button', { name: 'Check for updates', exact: true })).toBeDisabled()
  await page.evaluate(() => {
    if (!window.fixtureDesktopUpdates) throw new Error('Expected synthetic desktop fixture')
    window.fixtureDesktopUpdates.statusError = ''
  })
  await updates.getByRole('button', { name: 'Retry update status', exact: true }).click()
  await expect(updates.getByRole('button', { name: 'Check for updates', exact: true })).toBeEnabled()
  await expect(updates.getByRole('alert')).toHaveCount(0)
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.checks)).toBe(0)
})

test('software update controls fit both themes at desktop and mobile widths', async ({ page }, testInfo) => {
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  for (const theme of ['dark', 'light']) {
    await page.evaluate((value) => { document.documentElement.dataset.theme = value }, theme)
    for (const width of [1280, 900, 390]) {
      await page.setViewportSize({ width, height: 900 })
      await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeInViewport()
      expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(width)
      await page.screenshot({ path: testInfo.outputPath(`settings-${theme}-${width}.png`), animations: 'disabled' })
    }
  }
})

test('available updates keep download and restart actions explicit', async ({ page }) => {
  await mockSettings(page)
  await mockDesktop(page)
  await page.goto('/#/settings', { waitUntil: 'networkidle' })
  const updates = page.getByRole('region', { name: 'Software updates', exact: true })
  await updates.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate((status) => window.fixtureDesktopUpdates?.complete(status), {
    ...updateStatus('available', 'Porto 1.2.4 is available.'), availableVersion: '1.2.4',
  })
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.downloads)).toBe(0)
  await updates.getByRole('button', { name: 'Download Porto 1.2.4', exact: true }).click()
  await expect(updates.getByRole('button', { name: 'Restart and update', exact: true })).toBeEnabled()
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.downloads)).toBe(1)
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.installs)).toBe(0)
  await updates.getByRole('button', { name: 'Restart and update', exact: true }).click()
  await expect(updates.getByRole('status')).toContainText('Installing synthetic update.')
  await expect(updates.getByRole('button', { name: 'Check for updates', exact: true })).toBeDisabled()
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.installs)).toBe(1)
})
