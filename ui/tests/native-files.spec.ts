import { expect, test, type Page } from '@playwright/test'
import type { FileResource } from '../src/dataTypes'

async function mockNativeFiles(page: Page, disconnected = false) {
  page.on('pageerror', (error) => { throw error })
  const resource: FileResource = { kind: 'volume', name: 'synthetic-native', id: 'volume-1', readOnly: false }
  const submissions: Array<{ identity?: string; writable?: boolean }> = []
  const capabilityQueries: URLSearchParams[] = []
  let identity = 'synthetic-native-identity'
  let attachment = {
    id: 'synthetic-attachment', identity, resource,
    path: `/synthetic/native paths-\u03b4/${'long-directory/'.repeat(20)}files`,
    readOnly: true, state: disconnected ? 'disconnected' : 'connected', message: '',
  }
  let attachments = disconnected ? [attachment] : []
  await page.route('**/api/**', async (route) => {
    const url = new URL(route.request().url())
    const method = route.request().method()
    const json = (value: unknown, status = 200) => route.fulfill({ status, contentType: 'application/json', body: JSON.stringify(value) })
    if (url.pathname === '/api/files/capabilities') {
      capabilityQueries.push(url.searchParams)
      return json({ supported: true, driver: 'Synthetic native driver', message: 'Direct workload files.', fallback: 'Use verified archives if disconnected.', resource, identity })
    }
    if (url.pathname === '/api/files/attachments' && method === 'POST') {
      const request = route.request().postDataJSON()
      submissions.push(request)
      attachment = { ...attachment, identity, readOnly: !request.writable }
      attachments = [attachment]
      return json(attachment)
    }
    if (url.pathname === '/api/files/attachments/synthetic-attachment' && method === 'DELETE') {
      attachment = { ...attachment, state: 'detach-failed', message: 'Synthetic detach permission denied. Source data was not removed.' }
      attachments = [attachment]
      return json({ error: attachment.message }, 409)
    }
    const responses: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: true },
      '/api/docker/status': { available: true, enabled: true, context: 'porto' },
      '/api/docker/cleanup': { runs: [], enabled: false },
      '/api/docker/containers/snapshot': { instanceId: 'synthetic', revision: 1, available: true, stale: false, containers: [], capabilities: {} },
      '/api/docker/volumes': [{ name: resource.name, driver: 'local', mountpoint: '/synthetic/source', scope: 'local' }],
      '/api/docker/volumes/synthetic-native/storage': null,
      '/api/docker/volumes/synthetic-native/files': { resource, identity, path: '.', entries: [], readOnly: false, truncated: false },
      '/api/files/attachments': attachments,
    }
    return json(url.pathname in responses ? responses[url.pathname] : [])
  })
  await page.goto('/#/volumes')
  await page.getByText(resource.name, { exact: true }).first().click()
  await page.getByRole('tab', { name: 'Files', exact: true }).click()
  return { submissions, capabilityQueries, replaceIdentity: () => { identity = 'synthetic-recreated-identity'; resource.id = 'volume-2' } }
}

test('native writes require resource-bound consent and pass the capability identity', async ({ page }, testInfo) => {
  await page.clock.install()
  const fixture = await mockNativeFiles(page)
  const write = page.getByRole('checkbox', { name: /Explicit direct write access/ })
  await expect(write).not.toBeChecked()
  await write.check()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Open files in native file manager' }).click()
  await expect.poll(() => fixture.submissions.length).toBe(1)
  expect(fixture.submissions[0]).toMatchObject({ identity: 'synthetic-native-identity', writable: true })
  expect(fixture.capabilityQueries[0].get('name')).toBe('synthetic-native')
  fixture.replaceIdentity()
  await page.clock.fastForward(5001)
  await expect(write).not.toBeChecked()
  await page.clock.resume()
  await page.screenshot({ path: testInfo.outputPath('native-files-desktop.png'), fullPage: true, animations: 'disabled' })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.reload()
  await page.getByText('synthetic-native', { exact: true }).first().click()
  await page.getByRole('tab', { name: 'Files', exact: true }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Resource identity changed.' })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.screenshot({ path: testInfo.outputPath('native-files-mobile.png'), fullPage: true, animations: 'disabled' })
})

test('disconnected native folders cannot be reopened and failed detach stays visible', async ({ page }) => {
  await mockNativeFiles(page, true)
  await expect(page.getByRole('button', { name: 'Open attached files' })).toBeDisabled()
  await expect(page.getByRole('button', { name: 'Open files in native file manager' })).toBeDisabled()
  await page.getByRole('button', { name: 'Detach files' }).click()
  await expect(page.getByText(/detach-failed/)).toBeVisible()
  await expect(page.getByText(/Synthetic detach permission denied/).first()).toBeVisible()
})
