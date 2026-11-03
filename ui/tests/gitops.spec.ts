import { expect, test, type Page } from '@playwright/test'

const context = 'porto-synthetic'
const commit = 'a'.repeat(40)
const token = 'b'.repeat(32)
const scope = { context, namespace: 'default', controller: 'flux', controllerNamespace: 'flux-system' }

async function mockGitOps(page: Page, writable = true) {
  const requests: Array<{ path: string; body: Record<string, unknown> }> = []
  let applicationInstalled = false
  let sourceSaved = true
  const application = {
    ...scope, name: 'synthetic-app', kind: 'Kustomization',
    target: { context, namespace: 'default', group: 'kustomize.toolkit.fluxcd.io', version: 'v1', resource: 'kustomizations', name: 'synthetic-app', uid: 'synthetic-uid', resourceVersion: '7' },
    source: 'synthetic-source', desiredRevision: commit, sourceRevision: '', appliedRevision: '',
    phase: 'suspended', health: 'unknown', drift: 'unknown', dependenciesReady: true, conditions: [],
    automatic: false, selfHeal: true, prune: false, managed: true, inventory: [],
    configuration: { path: './deploy', serviceAccount: 'synthetic-reconciler', dependencies: [] },
    message: 'Native reconciliation is separate from application readiness.',
  }
  await page.route('**/api/**', async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    const baseline: Record<string, unknown> = {
      '/api/settings': { dockerEnabled: false, kubernetesEnabled: true, vmsEnabled: false, theme: 'dark' },
      '/api/docker/status': { enabled: false, available: false },
      '/api/docker/cleanup': { enabled: false, dockerEnabled: false, runs: [] },
      '/api/kubernetes/contexts': [{ name: context, cluster: context, namespace: 'default' }],
      '/api/kubernetes/clusters': [{ name: 'synthetic', state: 'running', context }],
      '/api/kubernetes/status': { available: true, enabled: true, context },
      '/api/kubernetes/admin/resource-names': { names: ['default', 'synthetic-other'] },
      '/api/kubernetes/gitops/controller': { ...scope, installed: true, ready: true, supported: true, managed: false, protected: true, canWrite: writable, canBootstrap: writable, bootstrapVersion: '2.10.0', capabilities: ['Kustomization', 'HelmRelease'], message: 'Supported native controller discovered.' },
      '/api/kubernetes/gitops/sources': sourceSaved ? [{ ...scope, name: 'synthetic-source', kind: 'git', url: 'https://git.example.invalid/synthetic.git', credentialStored: true }] : [],
      '/api/kubernetes/gitops/applications': applicationInstalled ? [application] : [],
      '/api/kubernetes/gitops/application': application,
      '/api/operations': [],
    }
    let value: unknown = baseline[path] ?? []
    if (request.method() === 'POST') {
      const body: Record<string, unknown> = request.postDataJSON()
      requests.push({ path, body })
      if (path.endsWith('/sources')) {
        sourceSaved = true
        value = { ...scope, name: 'synthetic-source', kind: 'git', url: 'https://git.example.invalid/synthetic.git', credentialStored: true }
      } else if (path.endsWith('/previews')) {
        value = { ...scope, token, action: body.action, name: body.name, kind: body.kind, differences: [{ target: application.target, kind: 'Kustomization', before: null, after: { spec: { suspend: true } }, managers: ['porto-gitops'] }], warnings: ['No force ownership or automatic data rollback.'], consequences: body.action === 'delete' ? ['PersistentVolumeClaim default/synthetic-data can lose persistent data.'] : [], expiresAt: '2099-01-01T00:00:00Z' }
      } else if (path.endsWith('/apply')) {
        applicationInstalled = true
        value = { id: 'c'.repeat(32), resource: `kubernetes:${context}`, action: 'gitops.install', status: 'queued', phase: 'waiting', startedAt: '2026-01-01T00:00:00Z' }
      }
    }
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify(value) })
  })
  await page.goto('/#/gitops')
  return { requests }
}

test('read-only GitOps identities cannot bootstrap or mutate and discovery does not install', async ({ page }) => {
  const { requests } = await mockGitOps(page, false)
  await expect(page.getByRole('heading', { name: 'GitOps applications', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Register GitOps application', exact: true })).toBeDisabled()
  await expect(page.getByText('Read-only or not authorized to mutate this controller')).toBeVisible()
  await page.getByText('Controller installation', { exact: true }).click()
  await expect(page.getByRole('button', { name: 'Preview controller lifecycle', exact: true })).toBeDisabled()
  expect(requests).toHaveLength(0)
})

test('private source registration clears credentials and application previews remain explicitly scoped', async ({ page }) => {
  const { requests } = await mockGitOps(page)
  await page.getByText('Connect a source', { exact: true }).click()
  await page.getByLabel('Source name', { exact: true }).fill('synthetic-source')
  await page.getByLabel('Source HTTPS URL', { exact: true }).fill('https://git.example.invalid/synthetic.git')
  await page.getByLabel('Use public source access', { exact: true }).uncheck()
  await page.getByLabel('Source username', { exact: true }).fill('synthetic-user')
  await page.getByLabel('Source password/token', { exact: true }).fill('synthetic-private-token')
  await page.getByRole('button', { name: 'Save scoped source', exact: true }).click()
  await expect(page.getByLabel('Source password/token', { exact: true })).toHaveValue('')
  expect(requests[0].body).toMatchObject({ ...scope, name: 'synthetic-source', password: 'synthetic-private-token' })
  await page.getByRole('button', { name: 'Register GitOps application', exact: true }).click()
  await page.getByLabel('Application name', { exact: true }).fill('synthetic-app')
  await page.getByRole('combobox', { name: 'Connected source', exact: true }).selectOption('synthetic-source')
  await page.getByLabel('Immutable Git commit / exact chart version', { exact: true }).fill(commit)
  await page.getByLabel('Reconciliation service account', { exact: true }).fill('synthetic-reconciler')
  await page.getByRole('button', { name: 'Preview GitOps operation', exact: true }).click()
  await expect(page.getByRole('region', { name: 'GitOps operation preview' })).toContainText('suspend')
  expect(requests[1].body).toMatchObject({ ...scope, action: 'install', kind: 'Kustomization', revision: commit, automatic: false, prune: false })
  page.once('dialog', (dialog) => dialog.dismiss())
  await page.getByRole('button', { name: 'Apply GitOps preview', exact: true }).click()
  expect(requests).toHaveLength(2)
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Apply GitOps preview', exact: true }).click()
  await expect.poll(() => requests.length).toBe(3)
  expect(requests[2].body).toEqual({ ...scope, preview: token, confirm: true, acknowledgeData: false })
  await expect(page.getByText('synthetic-private-token', { exact: false })).toHaveCount(0)
})

test('GitOps namespace changes discard previews rather than retargeting them', async ({ page }) => {
  await mockGitOps(page)
  await page.getByRole('button', { name: 'Register GitOps application', exact: true }).click()
  await page.getByLabel('Application name', { exact: true }).fill('synthetic-app')
  await page.getByRole('combobox', { name: 'Connected source', exact: true }).selectOption('synthetic-source')
  await page.getByLabel('Immutable Git commit / exact chart version', { exact: true }).fill(commit)
  await page.getByLabel('Reconciliation service account', { exact: true }).fill('synthetic-reconciler')
  await page.getByRole('button', { name: 'Preview GitOps operation', exact: true }).click()
  await expect(page.getByRole('region', { name: 'GitOps operation preview' })).toBeVisible()
  await page.getByLabel('Application namespace', { exact: true }).fill('synthetic-other')
  await expect(page.getByRole('region', { name: 'GitOps operation preview' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Apply GitOps preview', exact: true })).toHaveCount(0)
})

test('GitOps deletion requires extra storage consent and stays usable on narrow screens', async ({ page }) => {
  await mockGitOps(page)
  await page.getByRole('button', { name: 'Register GitOps application', exact: true }).click()
  await page.getByLabel('Application name', { exact: true }).fill('synthetic-app')
  await page.getByRole('combobox', { name: 'Connected source', exact: true }).selectOption('synthetic-source')
  await page.getByLabel('Immutable Git commit / exact chart version', { exact: true }).fill(commit)
  await page.getByLabel('Reconciliation service account', { exact: true }).fill('synthetic-reconciler')
  await page.getByRole('button', { name: 'Preview GitOps operation', exact: true }).click()
  page.once('dialog', (dialog) => dialog.accept())
  await page.getByRole('button', { name: 'Apply GitOps preview', exact: true }).click()
  await page.getByRole('button', { name: 'Close inspector', exact: true }).click()
  await page.getByText('synthetic-app', { exact: true }).click()
  await page.getByRole('combobox', { name: 'GitOps action', exact: true }).selectOption('delete')
  await page.getByRole('button', { name: 'Preview GitOps operation', exact: true }).click()
  await expect(page.getByRole('region', { name: 'GitOps operation preview' })).toContainText('PersistentVolumeClaim')
  await expect(page.getByRole('button', { name: 'Apply GitOps preview', exact: true })).toBeDisabled()
  await page.getByLabel('I acknowledge pruning, finalizer, namespace, CRD and storage consequences', { exact: true }).check()
  await expect(page.getByRole('button', { name: 'Apply GitOps preview', exact: true })).toBeEnabled()
  await page.setViewportSize({ width: 390, height: 844 })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true)
})
