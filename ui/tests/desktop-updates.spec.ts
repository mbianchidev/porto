import { expect, test, type Page } from '@playwright/test'
import type { DesktopUpdatePhase, DesktopUpdateStatus, IntegrationStatus, KillSwitchStatus, Settings } from '../src/types'

const settings: Settings = {
  cleanupLocalMerged: false, cleanupRemoteMerged: false, pruneRemoteTracking: false,
  protectedBranches: ['main'], sqlNotSoLiteEnabled: false, killSwitchEnabled: false,
  sendboxEnabled: false, dockerEnabled: false, dockerAutoPruneEnabled: false,
  kubernetesEnabled: false, vmsEnabled: false, interfaceDensity: 'compact',
  reduceMotion: true, terminalFontSize: 13, terminalLineHeight: 1.2,
  terminalCursorBlink: false, terminalScrollback: 1000, logRetentionDays: 7,
}

declare global {
  interface Window {
    fixtureDesktopUpdates?: {
      checks: number
      complete: (status: DesktopUpdateStatus) => void
      fail: (message: string) => void
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

async function mockDesktop(page: Page, phase: DesktopUpdatePhase = 'idle') {
  await page.addInitScript((initialStatus: DesktopUpdateStatus) => {
    let status = initialStatus
    const listeners = new Set<(status: DesktopUpdateStatus) => void>()
    let resolveCheck: ((status: DesktopUpdateStatus) => void) | null = null
    let rejectCheck: ((error: Error) => void) | null = null
    const fixture = {
      checks: 0,
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
    }
    window.fixtureDesktopUpdates = fixture
    window.portoDesktop = {
      openFiles: async () => {},
      getPreferences: async () => ({
        openAtLogin: false, keepInTray: false, automaticallyDownloadUpdates: false,
        loginItemSupported: true,
      }),
      setPreferences: async (preferences) => ({ ...preferences, loginItemSupported: true }),
      getUpdateStatus: async () => status,
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
      downloadUpdate: async () => status,
      restartAndInstallUpdate: async () => status,
    }
  }, updateStatus(phase, phase === 'unsupported' ? 'Update checks are available in installed Porto builds.' : undefined))
}

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
  await expect(page.getByRole('button', { name: 'Checking…', exact: true })).toBeDisabled()
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
  await expect(page.locator('.desktopBehaviorSettings').getByRole('alert')).toContainText('Synthetic release service unavailable.')
  await expect(page.getByRole('button', { name: 'Check for updates', exact: true })).toBeEnabled()
  await page.getByRole('button', { name: 'Check for updates', exact: true }).click()
  await page.evaluate((status) => window.fixtureDesktopUpdates?.complete(status),
    updateStatus('up-to-date', 'Porto 1.2.3 is up to date.'))
  await expect(page.getByText('Porto 1.2.3 is up to date.')).toBeVisible()
  expect(await page.evaluate(() => window.fixtureDesktopUpdates?.checks)).toBe(2)
})
