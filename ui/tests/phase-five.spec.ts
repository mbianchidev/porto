import { expect, test, type Page } from '@playwright/test'
import type { DeliveryApplication, DeliveryPreview, LocalOperation } from '../src/deliveryTypes'
import type { ImageAnalysisReport } from '../src/imageAnalysisTypes'

const digest = `sha256:${'a'.repeat(64)}`
const fixtureReport: ImageAnalysisReport = {
  id: 'a'.repeat(32), image: { digest, manifestDigest: digest, platform: 'linux/amd64' },
  status: 'succeeded', phase: 'complete', startedAt: '2026-01-01T00:00:00Z', completedAt: '2026-01-01T00:00:01Z',
  syftVersion: '1.55.0', grypeVersion: '0.121.0', cached: false, sbomId: 'b'.repeat(64),
  database: { state: 'ready', built: '2026-01-01T00:00:00Z', checksum: digest, location: '/synthetic/image-analysis/database', bytes: 1024, offline: true, maxAgeHours: 120 },
  baseImage: 'Unknown: ancestry is not verified.', distribution: 'synthetic 1', layers: [digest], warnings: [],
  packages: [{ id: 'pkg-one', name: 'synthetic-lib', version: '1', type: 'deb', licenses: ['MIT'], locations: ['/synthetic/lib'], layers: [digest] }],
  findings: [
    { id: 'CVE-2099-0001', packageId: 'pkg-one', package: 'synthetic-lib', version: '1', severity: 'High', fixState: 'fixed', fixedVersions: ['2'], matchTypes: ['exact-direct-match'], remediation: 'Update the recorded package to version 2 and rescan.' },
    { id: 'CVE-2099-0002', packageId: 'pkg-one', package: 'synthetic-lib', version: '1', severity: 'Low', fixState: 'not-fixed', fixedVersions: [], matchTypes: ['exact-direct-match'], remediation: 'No recorded fixed version.' },
  ],
}

function baseline(path: string): unknown {
  const values: Record<string, unknown> = {
    '/api/settings': { dockerEnabled: true, kubernetesEnabled: false, vmsEnabled: false, theme: 'dark' },
    '/api/docker/status': { enabled: true, available: true },
    '/api/docker/containers/snapshot': { instanceId: 'synthetic-runtime', revision: 1, available: true, stale: false, containers: [], events: [], capabilities: {} },
    '/api/docker/cleanup': { enabled: false, dockerEnabled: true, runs: [] },
    '/api/docker/images': [{ id: digest, repository: 'synthetic/image', tag: 'one', digest, platform: 'linux/amd64', size: '1 MiB', createdAt: '2026-01-01T00:00:00Z' }],
    '/api/kubernetes/contexts': [], '/api/kubernetes/clusters': [], '/api/operations': [],
  }
  return values[path] ?? []
}

async function mockAnalysis(page: Page, initial: ImageAnalysisReport) {
  let report = initial
  const requests: Array<{ path: string; body: unknown }> = []
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    let value: unknown = baseline(path)
    if (path === '/api/docker/analysis') value = [report]
    if (path === '/api/docker/analysis/database') value = report.database
    if (path === `/api/docker/analysis/${report.id}`) value = report
    if (path.endsWith('/analysis') && request.method() === 'POST') {
      requests.push({ path, body: request.postDataJSON() })
      report = { ...report, status: 'failed', phase: 'complete', error: 'Synthetic local Grype database is missing.', findings: undefined }
      value = report
    }
    if (path.endsWith('/export')) value = { spdxVersion: 'SPDX-2.3', packages: [] }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/images')
  await page.getByText('synthetic/image', { exact: true }).click()
  await page.getByRole('tab', { name: 'SBOM and vulnerabilities' }).click()
  return requests
}

test('local image analysis filters real finding fields and exports machine-readable SBOMs', async ({ page }) => {
  await mockAnalysis(page, fixtureReport)
  await expect(page.getByRole('table', { name: 'Grype vulnerability findings' })).toContainText('CVE-2099-0001')
  await page.getByRole('combobox', { name: 'Severity', exact: true }).selectOption('high')
  await expect(page.getByRole('table', { name: 'Grype vulnerability findings' })).not.toContainText('CVE-2099-0002')
  await page.getByLabel('Recorded fixes only').check()
  await expect(page.getByRole('table', { name: 'Grype vulnerability findings' })).toContainText('version 2')
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Export SPDX' }).click()
  expect((await download).suggestedFilename()).toContain('spdx-json.json')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})

test('image resource suggestions remain searchable and accept names outside the inventory', async ({ page }) => {
  await mockAnalysis(page, fixtureReport)
  await page.getByRole('button', { name: 'Close inspector', exact: true }).click()
  const input = page.getByLabel('Filter images', { exact: true })
  await expect.poll(() => input.evaluate((element) => {
    if (!(element instanceof HTMLInputElement)) throw new Error('Expected image resource filter')
    return Array.from(element.list?.options ?? [], (option) => option.value)
  })).toEqual(['synthetic/image:one'])
  await input.fill('synthetic/image:one')
  await expect(page.getByRole('listbox', { name: 'Docker images', exact: true }).getByRole('option')).toHaveCount(1)
  await input.fill('synthetic/not-listed:two')
  await expect(input).toHaveValue('synthetic/not-listed:two')
  await expect(page.getByRole('listbox', { name: 'Docker images', exact: true }).getByRole('option')).toHaveCount(0)
})

test('failed or unavailable analysis never becomes a clean zero', async ({ page }) => {
  const requests = await mockAnalysis(page, { ...fixtureReport, status: 'partial', findings: [], warnings: ['Synthetic OS package coverage is unavailable.'] })
  await expect(page.getByText('Incomplete, stale or failed analysis is not evidence of zero vulnerabilities.')).toBeVisible()
  await page.getByRole('button', { name: 'Analyze image', exact: true }).click()
  await expect.poll(() => requests.length).toBe(1)
  expect(requests[0].body).toMatchObject({ platform: 'linux/amd64', allowStale: false, force: false, maxAgeHours: 120 })
  await expect(page.getByRole('alert').filter({ hasText: 'Synthetic local Grype database is missing.' })).toBeVisible()
  await expect(page.getByText('Incomplete, stale or failed analysis is not evidence of zero vulnerabilities.')).toBeVisible()
})

test('Compose editor requires a redacted preview and explicit apply consent', async ({ page }) => {
  const requests: Array<{ path: string; body: unknown }> = []
  let applications: DeliveryApplication[] = []
  const preview: DeliveryPreview = {
    token: 'c'.repeat(32), projectId: 7, baseRevision: '', before: 'No recorded deployment.',
    after: '{"services":{"app":{"image":"registry.invalid/synthetic:one","environment":"[values hidden]"}}}',
    warnings: ['Persistent volumes are retained; database data is not rolled back.'], expiresAt: '2099-01-01T00:00:00Z',
  }
  const operation: LocalOperation = { id: 'd'.repeat(32), resource: 'project:7', action: 'compose.deploy', status: 'partial', phase: 'complete', error: 'Synthetic health check failed. Previous revision and volumes retained.', startedAt: '2026-01-01T00:00:00Z' }
  let admitted = false
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname
    let value: unknown = baseline(path)
    if (path === '/api/delivery') value = applications
    if (path === '/api/delivery/templates') value = []
    if (path === '/api/projects') value = [{ id: 7, name: 'synthetic-app', strategy: 'compose', path: '/synthetic/app', port: 41000, status: 'starting' }]
    if (path === '/api/operations') value = admitted ? [operation] : []
    if (request.method() === 'POST') {
      requests.push({ path, body: request.postDataJSON() })
      if (path === '/api/delivery/previews') {
        applications = [{ projectId: 7, name: 'synthetic-app', target: 'local', source: { kind: 'editor', git: { repository: '', ref: '', path: '' } }, currentRevision: '', pollSeconds: 0, webhookEnabled: false, failures: 0, drift: false, revisions: [] }]
        value = preview
      } else if (path.endsWith('/deployments')) {
        admitted = true
        applications = applications.map((application) => ({ ...application, lastOperation: operation.id, lastError: operation.error, failures: 1 }))
        value = operation
      }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/applications')
  const create = page.getByRole('button', { name: 'Create application', exact: true })
  await create.focus()
  await page.keyboard.press('Enter')
  await page.getByLabel('Application name', { exact: true }).fill('synthetic-app')
  await page.getByLabel('Compose YAML editor').fill('services:\n  app:\n    image: registry.invalid/synthetic:one\n')
  await page.getByLabel('Non-secret variables (JSON)').fill('{}')
  await page.getByLabel('Secret variables (JSON, omitted keys keep stored values)').fill('{"TOKEN":"synthetic-private-value"}')
  await page.getByRole('button', { name: 'Validate and preview', exact: true }).click()
  await expect(page.getByRole('region', { name: 'Application deployment preview' })).not.toContainText('synthetic-private-value')
  await expect(page.getByLabel('Secret variables (JSON, omitted keys keep stored values)')).toHaveValue('{}')
  expect(requests[0].body).toMatchObject({ target: 'local', pollSeconds: 0, webhookEnabled: false, secrets: { TOKEN: 'synthetic-private-value' } })
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Apply preview', exact: true }).click()
  expect(requests).toHaveLength(1)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Apply preview', exact: true }).click()
  await expect.poll(() => requests.length).toBe(2)
  expect(requests[1]).toEqual({ path: '/api/delivery/7/deployments', body: { preview: preview.token, confirm: true } })
  await expect(page.getByRole('table').filter({ hasText: 'compose.deploy' })).toContainText('partial')
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
  await page.keyboard.press('Escape')
  await expect(create).toBeFocused()
})
